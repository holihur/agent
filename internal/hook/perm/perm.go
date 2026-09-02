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

func checkDenied(cwd string, c agent.ToolCall) (bool, string) {
	p, err := permission.Load(cwd)
	if err != nil {
		return false, ""
	}
	ok, pat := p.IsDenied(c)
	return ok, pat
}
