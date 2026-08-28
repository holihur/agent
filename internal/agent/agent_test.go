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

// ---- 钩子集成 ----

func TestRunHookEventFlow(t *testing.T) {
	p := &fakeProvider{
		tools:  []tools.ToolDef{{Name: "shell", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		result: tools.ToolResult{Text: "hello"},
	}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewToolUse("tu_1", "shell", json.RawMessage(`{"command":"echo hi"}`))}}, StopReason: "tool_use"},
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("done")}}, StopReason: "end_turn"},
	}}
	h := NewHooks()
	var events []string
	var outcome RunOutcome
	h.OnRunStart(func(e UserInput) { events = append(events, "start") })
	h.OnBeforeLLM(func(s TurnStat) { events = append(events, "beforeLLM") })
	h.OnAfterLLM(func(s TurnStat) { events = append(events, "afterLLM:"+s.StopReason) })
	h.OnBeforeTool(func(c ToolCall) Decision { events = append(events, "beforeTool:"+c.Name); return Decision{} })
	h.OnAfterTool(func(o ToolOutcome) { events = append(events, "afterTool") })
	h.OnRunEnd(func(o RunOutcome) { outcome = o; events = append(events, "end") })

	ag := newTestAgent(t, fl, p)
	ag.Hooks = h

	answer, err := ag.Run(context.Background(), "run it")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "beforeLLM", "afterLLM:tool_use", "beforeTool:shell", "afterTool", "beforeLLM", "afterLLM:end_turn", "end"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
	if outcome.Answer != "done" || outcome.Err != nil || outcome.Turns != 2 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if answer != "done" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestRunHookDenyBackfillsAsError(t *testing.T) {
	p := &fakeProvider{
		tools: []tools.ToolDef{{Name: "shell", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewToolUse("tu_1", "shell", json.RawMessage(`{}`))}}, StopReason: "tool_use"},
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("understood, not running it")}}, StopReason: "end_turn"},
	}}
	h := NewHooks()
	var outcomes []ToolOutcome
	h.OnBeforeTool(func(c ToolCall) Decision { return Decision{Deny: true, Reason: "not allowed"} })
	h.OnAfterTool(func(o ToolOutcome) { outcomes = append(outcomes, o) })

	ag := newTestAgent(t, fl, p)
	ag.Hooks = h

	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || !outcomes[0].Denied || outcomes[0].Duration != 0 {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	tb := ag.Messages[2].Blocks[0]
	if !tb.IsError || tb.Content != "denied: not allowed" {
		t.Fatalf("denial not backfilled: %+v", tb)
	}
	if p.gotName != "" {
		t.Fatal("denied tool must not be executed")
	}
}

func TestRunNilHooksStillWorks(t *testing.T) {
	p := &fakeProvider{tools: []tools.ToolDef{{Name: "x", Description: "d", InputSchema: map[string]any{"type": "object"}}}}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("ok")}}, StopReason: "end_turn"},
	}}
	ag := newTestAgent(t, fl, p) // Hooks 保持 nil
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
}

// ---- Mutate 管道 ----

func TestMutatePipelinesChain(t *testing.T) {
	h := NewHooks()
	var seen string
	h.OnMutateUserInput(func(s string) string { return s + "-a" })
	h.OnMutateUserInput(func(s string) string { seen = s; return s + "-b" })
	h.OnMutateAnswer(func(s string) string { return "[" + s + "]" })

	if got := h.chainUserInput("x"); got != "x-a-b" {
		t.Fatalf("chained = %q", got)
	}
	if seen != "x-a" {
		t.Fatalf("pipeline order broken: %q", seen)
	}
	if got := h.chainAnswer("done"); got != "[done]" {
		t.Fatalf("answer = %q", got)
	}
}

func TestMutateToolInputReachesProvider(t *testing.T) {
	p := &fakeProvider{
		tools:  []tools.ToolDef{{Name: "shell", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		result: tools.ToolResult{Text: "ok"},
	}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewToolUse("tu_1", "shell", json.RawMessage(`{"command":"rm -rf /"}`))}}, StopReason: "tool_use"},
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("done")}}, StopReason: "end_turn"},
	}}
	h := NewHooks()
	h.OnMutateToolInput(func(c ToolCall) ToolCall {
		c.Input = json.RawMessage(`{"command":"echo safe"}`)
		return c
	})
	ag := newTestAgent(t, fl, p)
	ag.Hooks = h

	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if string(p.gotInput) != `{"command":"echo safe"}` {
		t.Fatalf("provider saw %s", p.gotInput)
	}
}

func TestMutateToolResultReachesBackfill(t *testing.T) {
	p := &fakeProvider{
		tools:  []tools.ToolDef{{Name: "shell", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		result: tools.ToolResult{Text: "secret: 12345"},
	}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewToolUse("tu_1", "shell", json.RawMessage(`{}`))}}, StopReason: "tool_use"},
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("ok")}}, StopReason: "end_turn"},
	}}
	h := NewHooks()
	h.OnMutateToolResult(func(r tools.ToolResult) tools.ToolResult {
		r.Text = "[redacted]"
		return r
	})
	ag := newTestAgent(t, fl, p)
	ag.Hooks = h

	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if tb := ag.Messages[2].Blocks[0]; tb.Content != "[redacted]" {
		t.Fatalf("backfill = %q", tb.Content)
	}
}

func TestMutateTurnRequestEphemeralAndFiltersTools(t *testing.T) {
	p := &fakeProvider{tools: []tools.ToolDef{
		{Name: "shell", Description: "d", InputSchema: map[string]any{"type": "object"}},
		{Name: "other", Description: "d", InputSchema: map[string]any{"type": "object"}},
	}}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("ok")}}, StopReason: "end_turn"},
	}}
	h := NewHooks()
	h.OnMutateTurnRequest(func(r TurnRequest) TurnRequest {
		r.System = "mutated-system"
		filtered := r.Tools[:0:0]
		filtered = append(filtered, r.Tools...)
		r.Tools = filtered[:1]
		r.Messages = nil
		return r
	})
	ag := newTestAgent(t, fl, p)
	ag.Hooks = h

	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	req := fl.called[0]
	if req.System != "mutated-system" {
		t.Fatalf("system = %q", req.System)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "shell" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	// 持久状态不受出站 mutate 影响(契约)。
	if len(ag.Messages) != 2 || ag.Messages[0].Blocks[0].Text != "hi" {
		t.Fatalf("persistent messages mutated: %+v", ag.Messages)
	}
}

func TestMutateAssistantBeforeHistory(t *testing.T) {
	p := &fakeProvider{tools: []tools.ToolDef{{Name: "x", Description: "d", InputSchema: map[string]any{"type": "object"}}}}
	fl := &fakeLLM{turns: []TurnResult{
		{Assistant: Message{Role: RoleAssistant, Blocks: []Block{NewText("raw")}}, StopReason: "end_turn"},
	}}
	h := NewHooks()
	h.OnMutateAssistant(func(m Message) Message {
		return Message{Role: m.Role, Blocks: []Block{NewText("clean")}}
	})
	ag := newTestAgent(t, fl, p)
	ag.Hooks = h

	answer, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "clean" {
		t.Fatalf("answer = %q", answer)
	}
	if last := ag.Messages[1]; last.Blocks[0].Text != "clean" {
		t.Fatalf("history = %+v", last)
	}
}
