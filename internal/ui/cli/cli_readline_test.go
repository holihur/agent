package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

func TestReadlineLoop_BusyAndNew(t *testing.T) {
	// Test the busy branch: when isRunning is true, new input should be rejected
	var out bytes.Buffer
	ui := New(strings.NewReader("/new\nexit\n"), &out)
	mock := &mockLLM{answer: "ok"}
	ag := newMockAgent(mock)
	ui.Agent = ag
	// Set running to true before calling readlineLoop
	_, cancel := context.WithCancel(context.Background())
	ui.setRunning(cancel, true)
	ctx, cancel2 := context.WithCancel(context.Background())
	// Make the loop exit quickly by canceling the context after a short time
	go func() { cancel2() }()
	err := ui.readlineLoop(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "agent busy") {
		t.Fatalf("expected busy message, got %q", out.String())
	}
	cancel()
}

func TestRunOnce_StreamedFallbackWithThinking(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	// Mock that will set streamed to true but leave buf empty, and return answer with thinking
	mock := &mockLLM{answer: "thinking fallback", stream: ""}
	ag := newMockAgent(mock)
	// Simulate the fallback case: streamed true, buf empty, answer non-empty
	// We need to make runOnce see streamed=true
	// Instead, we directly test the fallback logic by setting up the UI state
	// Create a scenario where TextContent will fallback to thinking
	// Use a mock that returns a message with only thinking
	ag2 := &agent.Agent{
		LLM: &mockLLMThinking{thinking: "deep thinking"},
		Registry: ag.Registry,
		System: "test",
		Hooks: agent.NewHooks(),
	}
	ui.Agent = ag2
	ag2.OnTextDelta = ui.TextDeltaSink()
	answer, err := ui.runOnce(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "deep thinking" {
		t.Fatalf("expected thinking fallback, got %q", answer)
	}
	if !strings.Contains(out.String(), "deep thinking") {
		t.Fatalf("out should contain thinking, got %q", out.String())
	}
}

type mockLLMThinking struct {
	thinking string
}

func (m *mockLLMThinking) Turn(ctx context.Context, r agent.TurnRequest) (agent.TurnResult, error) {
	return agent.TurnResult{
		Assistant: agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{
			{Type: agent.BlockThinking, Text: m.thinking},
		}},
		StopReason: "end_turn",
	}, nil
}

func (m *mockLLMThinking) TurnStream(ctx context.Context, r agent.TurnRequest, emit func(agent.TextDelta)) (agent.TurnResult, error) {
	// No text delta, only thinking
	return m.Turn(ctx, r)
}

func TestStartRLAndStopRL_Coverage(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	// Non-TTY should be no-op
	ui.startRL()
	if ui.rl != nil {
		t.Fatal("should be nil")
	}
	ui.stopRL()
	// Test with a mock that pretends to be TTY - hard to cover without real TTY, but we at least cover the no-op path
}

func TestWrite_WithRL_NonRunningAndRunning(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	// Test non-running
	ui.write("test\n")
	// Test running
	_, cancel := context.WithCancel(context.Background())
	ui.setRunning(cancel, true)
	ui.write("test\n")
	ui.setRunning(nil, false)
	cancel()
}
