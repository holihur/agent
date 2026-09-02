package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/tools"
)

type mockLLM struct {
	answer       string
	stream       string
	shouldCancel bool
}

func (m *mockLLM) Turn(ctx context.Context, r agent.TurnRequest) (agent.TurnResult, error) {
	if m.shouldCancel {
		return agent.TurnResult{}, context.Canceled
	}
	return agent.TurnResult{
		Assistant: agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText(m.answer)}},
		StopReason: "end_turn",
	}, nil
}

func (m *mockLLM) TurnStream(ctx context.Context, r agent.TurnRequest, emit func(agent.TextDelta)) (agent.TurnResult, error) {
	if m.shouldCancel {
		return agent.TurnResult{}, context.Canceled
	}
	if m.stream != "" {
		emit(agent.TextDelta{Text: m.stream})
	}
	return agent.TurnResult{
		Assistant: agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText(m.answer)}},
		StopReason: "end_turn",
	}, nil
}

func newMockAgent(llm *mockLLM) *agent.Agent {
	reg := tools.New()
	return &agent.Agent{
		LLM:      llm,
		Registry: reg,
		System:   "test",
		Hooks:    agent.NewHooks(),
	}
}

func TestTextDeltaSink(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	sink := ui.TextDeltaSink()
	if ui.streamed {
		t.Fatal("should not be streamed initially")
	}
	sink(agent.TextDelta{Text: "hello "})
	if !ui.streamed {
		t.Fatal("should be streamed after delta")
	}
	if ui.streamedBuf.String() != "hello " {
		t.Fatalf("buf = %q", ui.streamedBuf.String())
	}
	sink(agent.TextDelta{Text: "world"})
	if ui.streamedBuf.String() != "hello world" {
		t.Fatalf("buf = %q", ui.streamedBuf.String())
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestIsPromptActive(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	if ui.isPromptActive() {
		t.Fatal("should not be active")
	}
	ui.promptMu.Lock()
	ui.promptActive = true
	ui.promptMu.Unlock()
	if !ui.isPromptActive() {
		t.Fatal("should be active")
	}
}

func TestSetRunningAndIsRunning(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	if ui.isRunning() {
		t.Fatal("should not be running")
	}
	_, cancel := context.WithCancel(context.Background())
	ui.setRunning(cancel, true)
	if !ui.isRunning() {
		t.Fatal("should be running")
	}
	ui.setRunning(nil, false)
	if ui.isRunning() {
		t.Fatal("should not be running")
	}
	cancel()
}

func TestCancelRunning(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	ui.setRunning(cancel, true)
	ui.cancelRunning()
	if ctx.Err() == nil {
		t.Fatal("cancel should have been called")
	}
	ui.setRunning(nil, false)
	ui.cancelRunning()
}

func TestStartEscMonitor_NonTTY(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	stop := ui.startEscMonitor(context.Background(), func() {})
	stop()
}

func TestWrite_RawConversion(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	ui.write("a\nb")
	if out.String() != "a\nb" {
		t.Fatalf("non-running write = %q", out.String())
	}
	out.Reset()
	_, cancel := context.WithCancel(context.Background())
	ui.setRunning(cancel, true)
	ui.write("a\nb")
	if out.String() != "a\r\nb" {
		t.Fatalf("running write = %q", out.String())
	}
	ui.setRunning(nil, false)
	cancel()
}

func TestRunOnce_NonStreamed(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	mock := &mockLLM{answer: "hello from mock"}
	ag := newMockAgent(mock)
	ui.Agent = ag
	answer, err := ui.runOnce(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "hello from mock" {
		t.Fatalf("answer = %q", answer)
	}
	if !strings.Contains(out.String(), "hello from mock") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestRunOnce_Streamed(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	mock := &mockLLM{answer: "streamed answer", stream: "streamed answer"}
	ag := newMockAgent(mock)
	ui.Agent = ag
	ag.OnTextDelta = ui.TextDeltaSink()
	answer, err := ui.runOnce(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "streamed answer" {
		t.Fatalf("answer = %q", answer)
	}
	if ui.streamedBuf.String() != "streamed answer" {
		t.Fatalf("streamedBuf = %q", ui.streamedBuf.String())
	}
}

func TestRunOnce_Canceled(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	mock := &mockLLM{shouldCancel: true}
	ag := newMockAgent(mock)
	ui.Agent = ag
	_, err := ui.runOnce(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected canceled error")
	}
}

func TestRunOnce_NilAgent(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	_, err := ui.runOnce(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunOnce_AfterRunCallback(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	mock := &mockLLM{answer: "hi"}
	ag := newMockAgent(mock)
	ui.Agent = ag
	called := false
	ui.AfterRun = func(err error) { called = true }
	_, err := ui.runOnce(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("AfterRun not called")
	}
}

func TestExecuteAgent_Normal(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	mock := &mockLLM{answer: "ok"}
	ag := newMockAgent(mock)
	ui.Agent = ag
	ag.OnTextDelta = ui.TextDeltaSink()
	if err := ui.executeAgent(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if ui.isRunning() {
		t.Fatal("should not be running after")
	}
}

func TestExecuteAgent_Canceled(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	mock := &mockLLM{shouldCancel: true}
	ag := newMockAgent(mock)
	ui.Agent = ag
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ui.executeAgent(ctx, "hi")
	if err == nil {
		t.Fatal("expected canceled")
	}
}

func TestRunOnce_StreamedHasPrefixLogic(t *testing.T) {
	sb := "hello "
	answer := "hello world"
	if !strings.HasPrefix(answer, sb) {
		t.Fatalf("should be prefix")
	}
	missing := answer[len(sb):]
	if missing != "world" {
		t.Fatalf("missing = %q", missing)
	}
}
