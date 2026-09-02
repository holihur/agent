// Package verbose 实现打到 stderr 的 LLM 轮次与工具调用观测日志 hook。
package verbose

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

var verbose = flag.Bool("verbose", false, "print LLM turns and tool outcomes to stderr (hook)")
var displayToolcall = flag.Bool("display-toolcall", true, "print tool calls as [edit]/[read] etc. (set to false to hide)")
var displayThinking = flag.Bool("display-thinking", true, "print thinking blocks as [thinking] (set to false to hide)")

func init() {
	hook.Register("verbose", installVerbose)
}

// installVerbose 把 LLM 轮次与工具调用的观测日志挂到 stderr。
// -verbose 时打印 llm 轮次；tool/thinking 默认展示，可通过 -display-toolcall/-display-thinking 关闭
func installVerbose(h *agent.Hooks, _ hook.Deps) error {
	if *verbose {
		h.OnBeforeLLM(func(s agent.TurnStat) {
			fmt.Fprintf(os.Stderr, "[llm] turn %d: messages=%d tools=%d\r\n", s.Turn, s.Messages, s.Tools)
		})
		h.OnAfterLLM(func(s agent.TurnStat) {
			// After 日志紧随流式增量，可能停在行中；前置换行确保从行首开始，避免与正文同行导致“不从左边开始”
			fmt.Fprintf(os.Stderr, "\r\n[llm] turn %d: stop=%s blocks=%d\r\n", s.Turn, s.StopReason, s.Blocks)
		})
	}
	if *displayToolcall {
		// tool 调用打印为 [edit] / [read] 等形式（去掉通用 [tool] 前缀）
		h.OnBeforeTool(func(c agent.ToolCall) agent.Decision {
			// 简化输入预览，避免过长刷屏；按 rune 截断避免中文被切半，且压成单行避免阶梯错位
			preview := strings.TrimSpace(string(c.Input))
			preview = strings.ReplaceAll(preview, "\r\n", "\n")
			preview = strings.ReplaceAll(preview, "\r", "\n")
			preview = strings.ReplaceAll(preview, "\n", " ")
			if r := []rune(preview); len(r) > 300 {
				preview = string(r[:300]) + "…"
			}
			// 使用工具名本身作为括号前缀，符合期望的 [edit] 样式
			fmt.Fprintf(os.Stderr, "[%s] %s\r\n", c.Name, preview)
			return agent.Decision{}
		})
		h.OnAfterTool(func(o agent.ToolOutcome) {
			status := "ok"
			switch {
			case o.Denied:
				status = "denied"
			case o.IsError:
				status = "error"
			}
			// 同上，确保工具结果日志从行首开始
			fmt.Fprintf(os.Stderr, "\r\n[%s] -> %s (%s)\r\n", o.Name, status, o.Duration.Round(time.Millisecond))
		})
	}
	if *displayThinking {
		h.OnMutateAssistant(func(m agent.Message) agent.Message {
			for _, b := range m.Blocks {
				if b.Type == agent.BlockThinking {
					text := strings.TrimSpace(b.Text)
					if text == "" {
						continue
					}
					text = strings.ReplaceAll(text, "\r\n", "\n")
					text = strings.ReplaceAll(text, "\r", "\n")
					if r := []rune(text); len(r) > 500 {
						text = string(r[:500]) + "…"
					}
					text = strings.ReplaceAll(text, "\n", "\r\n")
					fmt.Fprintf(os.Stderr, "[thinking] %s\r\n", text)
				}
			}
			return m
		})
	}
	return nil
}
