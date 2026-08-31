package hook

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/holihur/agent/internal/agent"
)

var verbose = flag.Bool("verbose", false, "print LLM turns and tool outcomes to stderr (hook)")

func init() {
	Register("verbose", installVerbose)
}

// installVerbose 把 LLM 轮次与工具调用的观测日志挂到 stderr。
func installVerbose(h *agent.Hooks, _ Deps) error {
	if !*verbose {
		return nil
	}
	h.OnBeforeLLM(func(s agent.TurnStat) {
		fmt.Fprintf(os.Stderr, "[llm] turn %d: messages=%d tools=%d\n", s.Turn, s.Messages, s.Tools)
	})
	h.OnAfterLLM(func(s agent.TurnStat) {
		fmt.Fprintf(os.Stderr, "[llm] turn %d: stop=%s blocks=%d\n", s.Turn, s.StopReason, s.Blocks)
	})
	h.OnBeforeTool(func(c agent.ToolCall) agent.Decision {
		fmt.Fprintf(os.Stderr, "[tool] %s(%s)\n", c.Name, string(c.Input))
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
		fmt.Fprintf(os.Stderr, "[tool] %s -> %s (%s)\n", o.Name, status, o.Duration.Round(time.Millisecond))
	})
	return nil
}
