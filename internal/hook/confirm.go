package hook

import (
	"errors"
	"flag"

	"github.com/holihur/agent/internal/agent"
)

var confirmTool = flag.Bool("confirm-tool", false, "ask before every tool call (hook)")

func init() {
	Register("confirm-tool", installConfirmTool)
}

// installConfirmTool 把逐次放行 UI 接到 before_tool 裁决位;UI 默认拒绝。
func installConfirmTool(h *agent.Hooks, d Deps) error {
	if !*confirmTool {
		return nil
	}
	if d.UI == nil {
		return errors.New("confirm-tool needs a UI dependency")
	}
	h.OnBeforeTool(d.UI.ConfirmHook())
	return nil
}
