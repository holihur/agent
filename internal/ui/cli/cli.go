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
	"sync"
	"time"

	"github.com/chzyer/readline"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/tools"
)

// UI 是 CLI 交互实现:既是会话循环,也是 tools.Responder。
// Agent 由 cmd 在装配完成后注入(两阶段装配)。
type UI struct {
	Agent *agent.Agent

	// Model 是启动 banner 显示的当前模型名(由 cmd 注入最终解析值);
	// 空 = 不打印 banner(嵌入式等未配置模型的场景)。
	Model string

	// NewSession 非 nil 时接管 REPL 的 /new:开启新会话(如持久会话轮转),
	// 返回展示给用户的提示文本;nil 时由 UI 直接清空内存对话历史。
	NewSession func() string

	// AfterRun 非 nil 时,每次 runOnce 结束(无论成败)以该次运行错误回调
	// (如会话自动保存);成功时实参为 nil。
	AfterRun func(runErr error)

	in       io.Reader
	out      io.Writer
	rl       *readline.Instance // TTY 会话;nil = 逐行兜底
	scanner  *bufio.Scanner
	streamed    bool           // 本轮已流式输出(收尾不重复打印答案)
	streamedBuf strings.Builder // 累积流式增量，用于 streamed 为 true 但增量为空时的回退

	mu      sync.Mutex
	cancel  context.CancelFunc // 当前 agent 轮次的取消函数;nil = 空闲
	running bool               // 是否有 agent 正在执行

	writeMu sync.Mutex // 保护 rl.Write / os.Stdout 的并发写

	promptMu     sync.Mutex
	promptActive bool // readLine 是否正在等待用户输入（用于 ESC 监听避免抢输入）
}

func New(in io.Reader, out io.Writer) *UI {
	return &UI{
		in:      in,
		out:     out,
		scanner: bufio.NewScanner(in),
	}
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

// write 经 readline 输出(屏幕感知,避免行编辑重绘错位)。并发安全。
func (u *UI) write(s string) {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()
	if u.rl != nil {
		_, _ = u.rl.Write([]byte(s))
		return
	}
	_, _ = fmt.Fprint(u.out, s)
}

func (u *UI) readLine(prompt string) (string, error) {
	u.promptMu.Lock()
	u.promptActive = true
	u.promptMu.Unlock()
	defer func() {
		u.promptMu.Lock()
		u.promptActive = false
		u.promptMu.Unlock()
	}()
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

func (u *UI) isPromptActive() bool {
	u.promptMu.Lock()
	defer u.promptMu.Unlock()
	return u.promptActive
}

// TextDeltaSink 返回接给 Agent.OnTextDelta 的流式增量输出函数。
func (u *UI) TextDeltaSink() func(agent.TextDelta) {
	return func(d agent.TextDelta) {
		u.streamed = true
		u.streamedBuf.WriteString(d.Text)
		u.write(d.Text)
	}
}

// Run 启动 REPL。已分离为 readlineLoop(输入) 与 agentLoop(执行) 两个逻辑循环:
// readlineLoop 负责读行与指令分发,agentLoop 负责执行并可被 ESC 中断。
// exit/quit//exit//quit/Ctrl-D 退出;Ctrl-C 清空当前行;ESC 中断当前 agent 执行。
// "/" 命令(/help 等)由 slashcmd hook 在 Agent.Run 内拦截;/exit /quit 属
// REPL 循环控制,在这里直接退出。
func (u *UI) Run(ctx context.Context) error {
	u.startRL()
	defer u.stopRL()
	if u.Model != "" {
		u.write(fmt.Sprintf("agent — model: %s\n", u.Model))
	}
	return u.readlineLoop(ctx)
}

// readlineLoop 是输入循环:阻塞在 readLine,处理内置指令后将工作交由 agentLoop 执行。
// 与 agentLoop 分离后,ESC 可在 agent 执行期间通过上下文取消中断执行,而输入循环保持可恢复。
func (u *UI) readlineLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line, err := u.readLine("> ")
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			if u.isRunning() {
				u.cancelRunning()
				u.write("\n[esc] canceling...\n")
				continue
			}
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
		if line == "/new" {
			if u.isRunning() {
				u.write("agent busy, try again after current turn\n")
				continue
			}
			u.newSession()
			continue
		}
		if u.Agent == nil {
			u.write("error: agent not wired\n")
			continue
		}
		if u.isRunning() {
			u.write("agent busy, wait for current turn or press ESC to interrupt\n")
			continue
		}
		if err := u.executeAgent(ctx, line); err != nil {
			if errors.Is(err, context.Canceled) {
				// 区分外层信号取消（SIGINT，ctx 已取消）与内层 ESC 取消（仅 runCtx 取消）
				if ctx.Err() != nil {
					// 外层已取消（Ctrl-C 信号），按原语义退出 REPL
					return nil
				}
				if !u.streamed {
					u.write("[interrupted]\n")
				} else {
					u.write("\n")
				}
				continue
			}
			u.write(fmt.Sprintf("error: %v\n", err))
			continue
		}
	}
}

// executeAgent 在可取消的子上下文中执行一次 agent 问答,并附带 ESC 监听。
func (u *UI) executeAgent(parent context.Context, line string) error {
	runCtx, cancel := context.WithCancel(parent)
	u.setRunning(cancel, true)
	defer u.setRunning(nil, false)
	defer cancel()

	stopMonitor := u.startEscMonitor(runCtx, cancel)
	defer stopMonitor()

	u.streamed = false
	u.streamedBuf.Reset()
	_, err := u.runOnce(runCtx, line)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return err
	}
	return nil
}

func (u *UI) isRunning() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running
}

func (u *UI) setRunning(cancel context.CancelFunc, running bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cancel = cancel
	u.running = running
}

func (u *UI) cancelRunning() {
	u.mu.Lock()
	cancel := u.cancel
	u.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// startEscMonitor 在 TTY 下进入 raw 模式并监听 ESC/Ctrl-C,命中则取消 runCtx。
// 返回的 stop 函数会请求监听协程退出并等待其恢复终端状态。
// 非 TTY 或进入 raw 失败时返回空操作。
func (u *UI) startEscMonitor(runCtx context.Context, cancel context.CancelFunc) func() {
	if u.rl == nil {
		return func() {}
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}
	// MakeRaw 会清 OPOST 导致 \n 不回行首，verbose/流式输出会偏右；
	// 这里把输出回车映射加回来，保持 \n -> \r\n 的行为。
	if ts, err2 := unix.IoctlGetTermios(fd, unix.TIOCGETA); err2 == nil {
		ts.Oflag |= unix.OPOST | unix.ONLCR
		_ = unix.IoctlSetTermios(fd, unix.TIOCSETA, ts)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer term.Restore(fd, oldState)
		pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		buf := make([]byte, 1)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-done:
				return
			default:
			}
			// 若正在等待用户确认（ConfirmHook/Respond），避免抢掉 y/N 输入，
			// 但仍需监听 ESC 以支持中断确认
			if u.isPromptActive() {
				// 轻量探测：用 Poll 快速检查是否有 ESC/Ctrl-C，有则处理，无则让出
				n, _ := unix.Poll(pollFds, 20)
				if n == 0 {
					select {
					case <-runCtx.Done():
						return
					case <-done:
						return
					case <-time.After(20 * time.Millisecond):
						continue
					}
				}
				// 有输入，peek 一个字节
				nr, err := unix.Read(fd, buf)
				if err != nil || nr == 0 {
					continue
				}
				b := buf[0]
				if b == 27 {
					n2, _ := unix.Poll(pollFds, 20)
					if n2 > 0 {
						var drain [8]byte
						_, _ = unix.Read(fd, drain[:])
						continue
					}
					u.write("\n[esc] interrupted\n")
					cancel()
					return
				}
				if b == 3 {
					u.write("^C\n")
					cancel()
					return
				}
				// 非 ESC/Ctrl-C，说明是确认输入（y/N 等），放回给 readline
				if u.rl != nil {
					_, _ = u.rl.WriteStdin(buf[:nr])
				}
				continue
			}
			n, pollErr := unix.Poll(pollFds, 100)
			if pollErr != nil {
				if pollErr == unix.EINTR {
					continue
				}
				return
			}
			if n == 0 {
				continue
			}
			if pollFds[0].Revents&unix.POLLIN == 0 {
				continue
			}
			nr, readErr := unix.Read(fd, buf)
			if readErr != nil || nr == 0 {
				continue
			}
			b := buf[0]
			if b == 27 {
				n2, _ := unix.Poll(pollFds, 20)
				if n2 > 0 {
					var drain [8]byte
					_, _ = unix.Read(fd, drain[:])
					continue
				}
				u.write("\n[esc] interrupted\n")
				cancel()
				return
			}
			if b == 3 {
				u.write("^C\n")
				cancel()
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// newSession 处理 /new:NewSession 回调优先(持久会话轮转由装配层接管);
// 未装配时直接清空内存对话历史,不触碰持久会话文件。
func (u *UI) newSession() {
	if u.NewSession != nil {
		if note := u.NewSession(); note != "" {
			u.write(note + "\n")
		}
		return
	}
	if u.Agent == nil {
		u.write("error: agent not wired\n")
		return
	}
	u.Agent.Messages = nil
	u.write("session: new (history cleared)\n")
}

// runOnce 完成一次问答的收尾:流式期间已输出的文本不再重复打印;
// 错误时先补换行保持终端整洁。
func (u *UI) runOnce(ctx context.Context, line string) (string, error) {
	if u.Agent == nil {
		return "", errors.New("agent not wired")
	}
	u.streamed = false
	u.streamedBuf.Reset()
	answer, err := u.Agent.Run(ctx, line)
	if u.AfterRun != nil {
		u.AfterRun(err)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		if u.streamed {
			u.write("\n")
		}
		return "", err
	}
	if u.streamed {
		if u.streamedBuf.Len() == 0 && answer != "" {
			u.write(answer + "\n")
		} else if answer != "" && u.streamedBuf.String() != answer {
			sb := u.streamedBuf.String()
			// 若 answer 以已流式内容为前缀，仅补打缺失后缀，避免重复
			if strings.HasPrefix(answer, sb) {
				missing := answer[len(sb):]
				if missing != "" {
					u.write(missing + "\n")
				} else {
					u.write("\n")
				}
			} else if !strings.Contains(answer, sb) && sb != "" {
				u.write("\n" + answer + "\n")
			} else {
				u.write("\n")
			}
		} else {
			u.write("\n")
		}
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
