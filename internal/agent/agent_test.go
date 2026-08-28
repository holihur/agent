package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agent/internal/tools"
)

// ---- 测试替身 ----

type fakeLLM struct {
	turns  []TurnResult
	err    error
	called []TurnRequest
	loop   bool // true = 剧本耗尽后重复最后一个回合(测循环上限用)
}

func (f *fakeLLM) Turn(_ context.Context, r TurnRequest) (TurnResult, error) {
	f.called = append(f.called, r)
	if f.err != nil {
		return TurnResult{}, f.err
	}
	if len(f.turns) == 0 {
		return TurnResult{}, errors.New("fakeLLM: script exhausted")
	}
	t := f.turns[0]
	if !f.loop {
		f.turns = f.turns[1:]
	}
	return t, nil
}

type fakeProvider struct {
	tools    []tools.ToolDef
	result   tools.ToolResult
	err      error
	gotName  string
	gotInput json.RawMessage
}

func (f *fakeProvider) Namespace() string { return "local" }

func (f *fakeProvider) ListTools(_ context.Context) ([]tools.ToolDef, error) { return f.tools, nil }

func (f *fakeProvider) CallTool(_ context.Context, name string, input json.RawMessage) (tools.ToolResult, error) {
	f.gotName = name
	f.gotInput = append(json.RawMessage(nil), input...)
	return f.result, f.err
}

func newTestAgent(t *testing.T, llm LLM, p tools.Provider) *Agent {
	t.Helper()
	reg := tools.New()
	if err := reg.Register(p); err != nil {
		t.Fatal(err)
	}
	return &Agent{LLM: llm, Registry: reg}
}

// ---- 循环测试 ----

func TestRunToolCallRoundTrip(t *testing.T) {
	input := json.RawMessage(`{"expr":"3+5"}`)
	p := &fakeProvider{
		tools:  []tools.ToolDef{{Name: "calculator", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		result: tools.ToolResult{Text: "8"},
	}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewToolUse("tu_1", "calculator", input)}}, StopReason: "tool_use"},
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("3+5 = 8")}}, StopReason: "end_turn"},
	}}
	ag := newTestAgent(t, fl, p)

	got, err := ag.Run(context.Background(), "3+5 等于几")
	if err != nil {
		t.Fatal(err)
	}
	if got != "3+5 = 8" {
		t.Fatalf("answer = %q, want %q", got, "3+5 = 8")
	}
	if p.gotName != "calculator" || string(p.gotInput) != `{"expr":"3+5"}` {
		t.Fatalf("provider got name=%q input=%s", p.gotName, p.gotInput)
	}
	// 回填必须是 user 角色的 tool_result 块,且 tool_use_id 对齐。
	toolMsg := ag.Messages[2]
	if toolMsg.Role != RoleUser || len(toolMsg.Blocks) != 1 {
		t.Fatalf("tool message = %+v", toolMsg)
	}
	tb := toolMsg.Blocks[0]
	if tb.Type != BlockToolResult || tb.ToolUseID != "tu_1" || tb.Content != "8" || tb.IsError {
		t.Fatalf("tool result block = %+v", tb)
	}
	last := fl.called[1]
	if len(last.Messages) != 3 {
		t.Fatalf("second turn messages = %d, want 3", len(last.Messages))
	}
	if last.Messages[2].Blocks[0].Type != BlockToolResult {
		t.Fatalf("second turn last message = %+v", last.Messages[2])
	}
}

func TestRunToolErrorBackfilledNotFatal(t *testing.T) {
	p := &fakeProvider{
		tools: []tools.ToolDef{{Name: "boom", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		err:   errors.New("division by zero"),
	}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewToolUse("tu_9", "boom", json.RawMessage(`{}`))}}, StopReason: "tool_use"},
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("cannot do that")}}, StopReason: "end_turn"},
	}}
	ag := newTestAgent(t, fl, p)

	got, err := ag.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if got != "cannot do that" {
		t.Fatalf("answer = %q", got)
	}
	tb := ag.Messages[2].Blocks[0]
	if !tb.IsError || tb.Content != "error: division by zero" {
		t.Fatalf("error not backfilled as is_error: %+v", tb)
	}
}

func TestRunTooManyTurns(t *testing.T) {
	p := &fakeProvider{tools: []tools.ToolDef{{Name: "loop", Description: "d", InputSchema: map[string]any{"type": "object"}}}}
	fl := &fakeLLM{loop: true, turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewToolUse("tu", "loop", json.RawMessage(`{}`))}}, StopReason: "tool_use"},
	}}
	ag := newTestAgent(t, fl, p)

	if _, err := ag.Run(context.Background(), "go"); !errors.Is(err, ErrTooManyTurns) {
		t.Fatalf("err = %v, want ErrTooManyTurns", err)
	}
	// 保险丝恰好放行 maxTurns 轮。
	if n := len(fl.called); n != maxTurns {
		t.Fatalf("llm turns = %d, want %d", n, maxTurns)
	}
}

func TestRunLLMFailureFatal(t *testing.T) {
	p := &fakeProvider{tools: []tools.ToolDef{{Name: "x", Description: "d", InputSchema: map[string]any{"type": "object"}}}}
	fl := &fakeLLM{err: errors.New("network down")}
	ag := newTestAgent(t, fl, p)

	if _, err := ag.Run(context.Background(), "hi"); err == nil || errors.Is(err, ErrTooManyTurns) {
		t.Fatalf("err = %v, want wrapped network error", err)
	}
}

func TestRunStopToolUseWithoutToolBlocks(t *testing.T) {
	p := &fakeProvider{tools: []tools.ToolDef{{Name: "x", Description: "d", InputSchema: map[string]any{"type": "object"}}}}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("no tools here")}}, StopReason: "tool_use"},
	}}
	ag := newTestAgent(t, fl, p)

	if _, err := ag.Run(context.Background(), "hi"); err == nil {
		t.Fatal("expected error for tool_use stop without tool_use blocks")
	}
}

func TestTextContent(t *testing.T) {
	m := Message{Role: RoleAssistant, Blocks: []Block{NewText("a"), NewToolUse("i", "n", nil), NewText("b")}}
	if got := m.TextContent(); got != "a\nb" {
		t.Fatalf("TextContent = %q", got)
	}
}
