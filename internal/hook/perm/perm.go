package perm

import (
	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/permission"
)

func init() {
	hook.Register("perm", installPerm)
}

func installPerm(h *agent.Hooks, d hook.Deps) error {
	cwd := d.CWD
	h.OnBeforeTool(func(c agent.ToolCall) agent.Decision {
		if ok, pat := checkDenied(cwd, c); ok {
			return agent.Decision{Deny: true, Reason: "denied by permission " + pat}
		}
		return agent.Decision{}
	})
	return nil
}

// checkDenied 返回调用是否被 deny 规则命中。规则文件损坏/读取失败时 **fail-closed**
// （拒绝并提示），不再静默放行（安全评审 §L11 / §2.5）。
func checkDenied(cwd string, c agent.ToolCall) (bool, string) {
	p, err := permission.Load(cwd)
	if err != nil {
		return true, "permission rules unavailable: " + err.Error()
	}
	ok, pat := p.IsDenied(c)
	return ok, pat
}
