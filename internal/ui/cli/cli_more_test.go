package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunOnce_CoversAllBranches2(t *testing.T) {
	t.Run("non-streamed", func(t *testing.T) {
		var out bytes.Buffer
		ui := New(strings.NewReader(""), &out)
		mock := &mockLLM{answer: "hello"}
		ag := newMockAgent(mock)
		ui.Agent = ag
		answer, err := ui.runOnce(context.Background(), "hi")
		if err != nil {
			t.Fatal(err)
		}
		if answer != "hello" {
			t.Fatalf("got %q", answer)
		}
	})
	t.Run("RunOnce", func(t *testing.T) {
		var out bytes.Buffer
		ui := New(strings.NewReader(""), &out)
		mock := &mockLLM{answer: "runonce"}
		ag := newMockAgent(mock)
		ui.Agent = ag
		if err := ui.RunOnce(context.Background(), "test"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "runonce") {
			t.Fatalf("out = %q", out.String())
		}
	})
	t.Run("newSession nil agent", func(t *testing.T) {
		ui := New(strings.NewReader(""), &bytes.Buffer{})
		ui.Agent = nil
		ui.newSession()
	})
	t.Run("startRL stopRL", func(t *testing.T) {
		ui := New(strings.NewReader(""), &bytes.Buffer{})
		ui.startRL()
		if ui.rl != nil {
			t.Fatal("should be nil for non-TTY")
		}
		ui.stopRL()
	})
	t.Run("preview", func(t *testing.T) {
		if got := preview("short"); got != "short" {
			t.Fatalf("got %q", got)
		}
		long := strings.Repeat("a", 100)
		if got := preview(long); len([]rune(got)) != 81 {
			t.Fatalf("preview long len %d", len([]rune(got)))
		}
	})
}

func TestStartEscMonitor_CoversNoOp2(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	stop := ui.startEscMonitor(context.Background(), func() {})
	stop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop2 := ui.startEscMonitor(ctx, func() {})
	stop2()
}

func TestReadlineLoopBusyAndNew(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader("test\nexit\n"), &out)
	mock := &mockLLM{answer: "ok"}
	ag := newMockAgent(mock)
	ui.Agent = ag
	_, cancel := context.WithCancel(context.Background())
	ui.setRunning(cancel, true)
	ctx, cancel2 := context.WithCancel(context.Background())
	go func() { cancel2() }()
	_ = ui.readlineLoop(ctx)
	cancel()
}
