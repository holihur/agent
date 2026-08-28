// Package cli 实现 CLI REPL 交互层:
//
//   - 会话循环:读问题 → Agent.Run → 打印回答
//   - tools.Responder:MRTR 追问时在终端逐字段收集用户作答
//
// 隔离目标(设计 v4):将来加 TUI/API = 新增 internal/ui/<mode> 包
// + cmd 里一个 case,agent/tools/llm/mcp 四包零改动。
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"agent/internal/agent"
	"agent/internal/tools"
)

// UI 是 CLI 交互实现:既是会话循环,也是 tools.Responder。
type UI struct {
	// Agent 由 cmd 在装配完成后注入(Responder 先于 Agent 可用的两阶段装配)。
	Agent *agent.Agent

	in  *bufio.Scanner
	out io.Writer
}

func New(in io.Reader, out io.Writer) *UI {
	return &UI{in: bufio.NewScanner(in), out: out}
}

// Run 启动 REPL:"> " 提示 → 读行 → agent.Run → 打印。exit/quit/EOF 退出。
// 单轮致命错误只打印不退出(会话继续);用户中断(ctx 取消)则退出。
func (u *UI) Run(ctx context.Context) error {
	for {
		fmt.Fprint(u.out, "> ")
		if !u.in.Scan() {
			fmt.Fprintln(u.out)
			return nil // EOF
		}
		line := strings.TrimSpace(u.in.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if u.Agent == nil {
			fmt.Fprintln(u.out, "error: agent not wired")
			continue
		}
		answer, err := u.Agent.Run(ctx, line)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			fmt.Fprintf(u.out, "error: %v\n", err)
			continue
		}
		fmt.Fprintln(u.out, answer)
	}
}

// Respond 实现 tools.Responder:终端逐 Prompt、逐字段收集作答。
// 输入流关闭 = 拒绝作答(error → 上层转 is_error 工具结果)。
func (u *UI) Respond(_ context.Context, req tools.InputRequest) ([]tools.InputResponse, error) {
	var resps []tools.InputResponse
	for _, prompt := range req.Prompts {
		fmt.Fprintf(u.out, "\n[%s needs input] %s\n", req.Tool, prompt.Message)
		content := map[string]any{}
		for _, f := range prompt.Fields {
			mark := ""
			if f.Required {
				mark = " (required)"
			}
			fmt.Fprintf(u.out, "  %s%s: ", f.Name, mark)
			if !u.in.Scan() {
				return nil, fmt.Errorf("input stream closed")
			}
			v := strings.TrimSpace(u.in.Text())
			if v == "" && !f.Required {
				continue
			}
			content[f.Name] = v
		}
		resps = append(resps, tools.InputResponse{Key: prompt.Key, Content: content})
	}
	return resps, nil
}
