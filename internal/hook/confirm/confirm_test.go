package confirm

import (
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

func TestInstallConfirmTool_Disabled(t *testing.T) {
	*confirmTool = false
	defer func() { *confirmTool = false }()
	h := agent.NewHooks()
	if err := installConfirmTool(h, hook.Deps{}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallConfirmTool_EnabledNoUI(t *testing.T) {
	*confirmTool = true
	defer func() { *confirmTool = false }()
	h := agent.NewHooks()
	err := installConfirmTool(h, hook.Deps{UI: nil})
	if err == nil {
		t.Fatal("expected error when UI is nil")
	}
}

func TestInstallConfirmTool_EnabledWithUI(t *testing.T) {
	*confirmTool = true
	defer func() { *confirmTool = false }()
	h := agent.NewHooks()
	mockUI := &mockConfirmUI{}
	if err := installConfirmTool(h, hook.Deps{UI: mockUI}); err != nil {
		t.Fatal(err)
	}
}

type mockConfirmUI struct{}

func (m *mockConfirmUI) ConfirmHook() func(agent.ToolCall) agent.Decision {
	return func(c agent.ToolCall) agent.Decision { return agent.Decision{} }
}
