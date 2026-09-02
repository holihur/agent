package agent

import (
	"context"
	"testing"

	"github.com/holihur/agent/internal/tools"
)

func TestExecTools_Canceled(t *testing.T) {
	// Setup an agent with a registry that will be canceled
	reg := tools.New()
	// Register a dummy provider that will block
	ag := &Agent{
		LLM:      &fakeLLM2{},
		Registry: reg,
		Hooks:    NewHooks(),
	}
	// Create a message with a tool_use
	msg := Message{
		Role: RoleAssistant,
		Blocks: []Block{
			NewToolUse("id1", "test_tool", []byte(`{}`)),
			NewToolUse("id2", "test_tool2", []byte(`{}`)),
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err := ag.execTools(ctx, msg, 0)
	if err == nil {
		t.Fatal("expected cancel error")
	}
}

func TestExecTools_Denied(t *testing.T) {
	reg := tools.New()
	ag := &Agent{
		LLM:      &fakeLLM2{},
		Registry: reg,
		Hooks:    NewHooks(),
	}
	// Add a hook that denies the tool
	ag.Hooks.OnBeforeTool(func(c ToolCall) Decision {
		return Decision{Deny: true, Reason: "denied for test"}
	})
	msg := Message{
		Role: RoleAssistant,
		Blocks: []Block{
			NewToolUse("id1", "denied_tool", []byte(`{}`)),
		},
	}
	result, err := ag.execTools(context.Background(), msg, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Blocks) != 1 || !result.Blocks[0].IsError {
		t.Fatalf("expected denied tool result, got %+v", result)
	}
}

type fakeLLM2 struct{}

func (f *fakeLLM2) Turn(ctx context.Context, r TurnRequest) (TurnResult, error) {
	return TurnResult{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("hi")}}, StopReason: "end_turn"}, nil
}
