// Package cli 实现 CLI REPL 交互层:
//
//   - 会话循环:readline 行编辑(方向键/历史/Ctrl-C) → Agent.Run → 打印回答
//   - tools.Responder:MRTR 追问时在终端逐字段收集用户作答
//
// 隔离目标(设计 v4):将来加 TUI/API = 新增 internal/ui/<mode> 包
// + cmd 里一个 case,agent/tools/llm/mcp 四包零改动。
//
// 终端形态自动降级:stdin 为 TTY 时启用 readline(raw mode);
// 管道/一次性(-q)/测试环境退回逐行扫描,不触碰终端状态。
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chzyer/readline"
	"golang.org/x/term"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/tools"
	"github.com/holihur/agent/pkg/streamdown"
)

// UI 是 CLI 交互实现:既是会话循环,也是 tools.Responder。
// Agent 由 cmd 在装配完成后注入(两阶段装配)。
type UI struct {
	Agent *agent.Agent

	// AfterRun 非 nil 时,每次 runOnce 结束(无论成败)以该次运行错误回调
	// (如会话自动保存);成功时实参为 nil。
	AfterRun func(runErr error)

	in       io.Reader
	out      io.Writer
	rl       *readline.Instance // TTY 会话;nil = 逐行兜底
	scanner  *bufio.Scanner
	streamed bool // 本轮已流式输出(收尾不重复打印答案)

	// markdown 渲染(mdOn 时开启):每个回答一个渲染生命周期。
	// 流式增量经 io.Pipe 喂给渲染器,渲染输出走 readline 感知的 write。
	mdOn   bool
	md     *streamdown.Renderer
	mdPipe *io.PipeWriter
	mdDone chan struct{}
}

func New(in io.Reader, out io.Writer) *UI {
	return &UI{
		in:      in,
		out:     out,
		scanner: bufio.NewScanner(in),
		mdOn:    terminalWidth(out) > 0, // auto:输出为 TTY 时默认开启
	}
}

// SetMarkdown 开关回答的 markdown 终端渲染(默认按输出是否 TTY 自动决定)。
func (u *UI) SetMarkdown(on bool) { u.mdOn = on }

// terminalWidth 返回输出终端宽度;非 TTY/不可测时为 0(渲染器回落 80)。
func terminalWidth(out io.Writer) int {
	f, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return 0
	}
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

// startRL 在 TTY 上启动 readline。延迟到 Run 才启动:
// 一次性(-q)模式不碰终端状态,避免进程退出后终端残留 raw mode。
func (u *UI) startRL() {
	rc, ok := u.in.(io.ReadCloser)
	if !ok || !readline.DefaultIsTerminal() {
		return
	}
	rl, err := readline.NewEx(&readline.Config{
		Prompt:       "> ",
		Stdin:        rc,
		Stdout:       u.out,
		HistoryLimit: 500,
	})
	if err != nil {
		return // 启动失败保持兜底路径
	}
	u.rl = rl
}

func (u *UI) stopRL() {
	if u.rl != nil {
		_ = u.rl.Close()
		u.rl = nil
	}
}

// write 经 readline 输出(屏幕感知,避免行编辑重绘错位)。
func (u *UI) write(s string) {
	if u.rl != nil {
		_, _ = u.rl.Write([]byte(s))
		return
	}
	_, _ = fmt.Fprint(u.out, s)
}

func (u *UI) readLine(prompt string) (string, error) {
	if u.rl != nil {
		u.rl.SetPrompt(prompt)
		return u.rl.Readline()
	}
	fmt.Fprint(u.out, prompt)
	if !u.scanner.Scan() {
		return "", io.EOF
	}
	return u.scanner.Text(), nil
}

// TextDeltaSink 返回接给 Agent.OnTextDelta 的增量渲染函数。
// markdown 开启时增量喂给渲染器(流式);否则原文直写。
func (u *UI) TextDeltaSink() func(agent.TextDelta) {
	return func(d agent.TextDelta) {
		u.streamed = true
		if u.md != nil && u.mdPipe != nil {
			_, _ = u.mdPipe.Write([]byte(d.Text))
			return
		}
		u.write(d.Text)
	}
}

// mdBegin 开启一个回答的 markdown 渲染会话:渲染器 + io.Pipe + 消费 goroutine。
// 渲染输出经 u.write 写出(readline 感知);宽度取输出终端,非 TTY 回落 80。
func (u *UI) mdBegin() {
	if !u.mdOn {
		return
	}
	cfg := streamdown.DefaultConfig()
	cfg.Width = terminalWidth(u.out)
	var err error
	u.md, err = streamdown.New(mdWriter{u}, cfg)
	if err != nil {
		u.md = nil
		return
	}
	pr, pw := io.Pipe()
	u.mdPipe = pw
	u.mdDone = make(chan struct{})
	go func() {
		defer close(u.mdDone)
		_ = u.md.Render(pr)
	}()
}

// mdEnd 结束当前回答:关闭管道等待渲染完成;未流式时把整段回答也渲染一遍;
// 最后 Tidyup 复位终端。任何路径都不允许泄漏 goroutine。
func (u *UI) mdEnd(answer string, streamed bool) {
	if u.md == nil {
		return
	}
	if u.mdPipe != nil {
		_ = u.mdPipe.Close()
		u.mdPipe = nil
	}
	if u.mdDone != nil {
		<-u.mdDone
		u.mdDone = nil
	}
	if !streamed && answer != "" {
		_ = u.md.RenderString(answer)
	}
	u.md.Tidyup()
	u.md = nil
}

// mdWriter 把渲染器输出转发到 readline 感知的 write。
type mdWriter struct{ u *UI }

func (w mdWriter) Write(p []byte) (int, error) {
	w.u.write(string(p))
	return len(p), nil
}

// Run 启动 REPL:"> " 提示 → 读行 → agent.Run → 打印。
// exit/quit//exit//quit/Ctrl-D 退出;Ctrl-C 清空当前行;单轮致命错误只打印不退出。
// "/" 命令(/help 等)由 slashcmd hook 在 Agent.Run 内拦截;/exit /quit 属
// REPL 循环控制,在这里直接退出。
func (u *UI) Run(ctx context.Context) error {
	u.startRL()
	defer u.stopRL()
	for {
		line, err := u.readLine("> ")
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			u.write("^C\n")
			continue
		case errors.Is(err, io.EOF):
			u.write("\n")
			return nil
		case err != nil:
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" || line == "/exit" || line == "/quit" {
			return nil
		}
		if u.Agent == nil {
			u.write("error: agent not wired\n")
			continue
		}
		u.streamed = false
		_, err = u.runOnce(ctx, line)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			u.write(fmt.Sprintf("error: %v\n", err))
			continue
		}
	}
}

// runOnce 完成一次问答的渲染收尾:流式期间已输出的文本不再重复打印;
// 错误时先补换行保持终端整洁。markdown 开启时整段回答由渲染器输出。
func (u *UI) runOnce(ctx context.Context, line string) (string, error) {
	if u.Agent == nil {
		return "", errors.New("agent not wired")
	}
	u.streamed = false
	u.mdBegin()
	mdActive := u.md != nil
	answer, err := u.Agent.Run(ctx, line)
	u.mdEnd(answer, u.streamed)
	if u.AfterRun != nil {
		u.AfterRun(err)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		if u.streamed && !mdActive {
			u.write("\n") // 渲染器输出以换行收尾,原文直出才需补行
		}
		return "", err
	}
	if mdActive {
		return answer, nil // 已由渲染器输出(流式或整段),无需再写
	}
	if u.streamed {
		u.write("\n")
	} else {
		u.write(answer + "\n")
	}
	return answer, nil
}

// RunOnce 一次性问答(-q 模式):不启动 readline,直接渲染。
func (u *UI) RunOnce(ctx context.Context, question string) error {
	_, err := u.runOnce(ctx, question)
	return err
}

// ConfirmHook 返回工具确认钩子:每次工具调用前向用户请求放行。
// 默认拒绝 —— 直接回车或任何非 y 答复都会拒绝执行。
func (u *UI) ConfirmHook() func(agent.ToolCall) agent.Decision {
	return func(c agent.ToolCall) agent.Decision {
		v, err := u.readLine(fmt.Sprintf("allow %s(%s)? [y/N] ", c.Name, preview(string(c.Input))))
		if err != nil {
			return agent.Decision{Deny: true, Reason: "input closed"}
		}
		switch strings.TrimSpace(strings.ToLower(v)) {
		case "y", "yes":
			return agent.Decision{}
		default:
			return agent.Decision{Deny: true, Reason: "user denied"}
		}
	}
}

func preview(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

// Respond 实现 tools.Responder:终端逐 Prompt、逐字段收集作答。
// 输入流关闭 = 拒绝作答(error → 上层转 is_error 工具结果)。
func (u *UI) Respond(_ context.Context, req tools.InputRequest) ([]tools.InputResponse, error) {
	var resps []tools.InputResponse
	for _, prompt := range req.Prompts {
		u.write(fmt.Sprintf("\n[%s needs input] %s\n", req.Tool, prompt.Message))
		content := map[string]any{}
		for _, f := range prompt.Fields {
			mark := ""
			if f.Required {
				mark = " (required)"
			}
			v, err := u.readLine(fmt.Sprintf("  %s%s: ", f.Name, mark))
			if err != nil {
				return nil, fmt.Errorf("input closed: %w", err)
			}
			v = strings.TrimSpace(v)
			if v == "" && !f.Required {
				continue
			}
			content[f.Name] = v
		}
		resps = append(resps, tools.InputResponse{Key: prompt.Key, Content: content})
	}
	return resps, nil
}
