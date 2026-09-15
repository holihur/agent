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

// humanDuration 输出紧凑可读的耗时：<1s 精确到 ms，≥1s 保留 0.1s 精度。
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

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
			// token 用量用 i/o/c 简写：i=未命中输入，o=输出，c=缓存输入(读+写)
			cache := ""
			if s.Usage.CacheRead > 0 || s.Usage.CacheCreate > 0 {
				cache = fmt.Sprintf("+%d", s.Usage.CacheRead+s.Usage.CacheCreate)
			}
			fmt.Fprintf(os.Stderr, "\r\n[llm] turn %d: stop=%s blocks=%d i=%d o=%d c=%s %s\r\n",
				s.Turn, s.StopReason, s.Blocks, s.Usage.Input, s.Usage.Output, cache, humanDuration(s.Duration))
		})
	}
	if *displayToolcall {
		// tool 调用打印为 [edit] / [read] 等形式（去掉通用 [tool] 前缀）；
		// 前后两段写同一行：Before 不换行，After 用 \r 回到行首原地补上状态与耗时
		pending := 0
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
			fmt.Fprintf(os.Stderr, "[%s] %s", c.Name, preview)
			pending = len([]rune("[" + c.Name + "] " + preview))
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
			line := fmt.Sprintf("[%s] -> %s (%s)", o.Name, status, humanDuration(o.Duration))
			// 用空格覆盖残留的旧字符，避免原输入预览比结果行更长时留下尾巴
			if pad := pending - len([]rune(line)); pad > 0 {
				line += strings.Repeat(" ", pad)
			}
			fmt.Fprintf(os.Stderr, "\r%s\r\n", line)
			pending = 0
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
