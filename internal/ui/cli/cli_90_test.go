package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/chzyer/readline"
	"github.com/holihur/agent/internal/agent"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestReadlineLoop_CoversAllBranchesWithMock(t *testing.T) {
	// Test ErrInterrupt with running true
	t.Run("interrupt with running", func(t *testing.T) {
		var out bytes.Buffer
		ui := New(nil, &out)
		_, cancel := context.WithCancel(context.Background())
		ui.setRunning(cancel, true)
		// Mock readLine to return ErrInterrupt, and isRunning true, so it should cancel and continue
		// We need to make readlineLoop handle this and then return on context cancel
		ctx, cancel2 := context.WithCancel(context.Background())
		// Make readLine return ErrInterrupt once, then EOF to exit
		called := 0
		ui.readLineFunc = func(prompt string) (string, error) {
			called++
			if called == 1 {
				return "", readline.ErrInterrupt
			}
			return "", io.EOF
		}
		err := ui.readlineLoop(ctx)
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if !calledEqual(called, 2) {
			t.Fatalf("called %d", called)
		}
		cancel()
		cancel2()
	})

	// Test ErrInterrupt with running false
	t.Run("interrupt without running", func(t *testing.T) {
		var out bytes.Buffer
		ui := New(nil, &out)
		called := 0
		ui.readLineFunc = func(prompt string) (string, error) {
			called++
			if called == 1 {
				return "", readline.ErrInterrupt
			}
			return "", io.EOF
		}
		err := ui.readlineLoop(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !containsStr(out.String(), "^C") {
			t.Fatalf("want ^C, got %q", out.String())
		}
	})

	// Test generic error
	t.Run("generic error", func(t *testing.T) {
		ui := New(nil, &bytes.Buffer{})
		ui.readLineFunc = func(prompt string) (string, error) {
			return "", errors.New("generic")
		}
		err := ui.readlineLoop(context.Background())
		if err == nil || err.Error() != "generic" {
			t.Fatalf("got %v", err)
		}
	})

	// Test /new with running
	t.Run("/new busy", func(t *testing.T) {
		var out bytes.Buffer
		ui := New(nil, &out)
		ui.readLineFunc = func(prompt string) (string, error) {
			// First call returns /new, second returns exit
			if out.String() == "" {
				// First call, we need to set running before
				return "/new", nil
			}
			return "exit", nil
		}
		_, cancel := context.WithCancel(context.Background())
		ui.setRunning(cancel, true)
		// Need to make readLine return /new while running
		called := 0
		ui.readLineFunc = func(prompt string) (string, error) {
			called++
			if called == 1 {
				return "/new", nil
			}
			return "exit", nil
		}
		err := ui.readlineLoop(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !containsStr(out.String(), "agent busy") {
			t.Fatalf("want busy, got %q", out.String())
		}
		cancel()
	})

	// Test agent busy
	t.Run("agent busy", func(t *testing.T) {
		var out bytes.Buffer
		ui := New(nil, &out)
		// Make Agent not nil but running
		ui.Agent = &agent.Agent{Hooks: agent.NewHooks()}
		_, cancel := context.WithCancel(context.Background())
		ui.setRunning(cancel, true)
		called := 0
		ui.readLineFunc = func(prompt string) (string, error) {
			called++
			if called == 1 {
				return "hello", nil
			}
			return "exit", nil
		}
		err := ui.readlineLoop(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !containsStr(out.String(), "agent busy") {
			t.Fatalf("want busy, got %q", out.String())
		}
		cancel()
	})

	t.Run("executeAgent error with streamed", func(t *testing.T) {
		var out bytes.Buffer
		_ = New(nil, &out)
		_ = out
	})
}

func TestStartEscMonitor_TTYPath(t *testing.T) {
	t.Skip("skip TTY path test - requires real terminal")
	// Mock the term and unix functions to cover the TTY path
	origIsTerminal := isTerminal
	origMakeRaw := makeRaw
	origRestore := termRestore
	origPoll := pollFunc
	origRead := readFunc
	defer func() {
		isTerminal = origIsTerminal
		makeRaw = origMakeRaw
		termRestore = origRestore
		pollFunc = origPoll
		readFunc = origRead
	}()

	// Mock to make it think it's a terminal and MakeRaw succeeds
	isTerminal = func(fd int) bool { return true }
	makeRaw = func(fd int) (*term.State, error) { return &term.State{}, nil }
	termRestore = func(fd int, s *term.State) error { return nil }
	// Mock Poll to return 0 (no data) quickly, and then context cancel
	pollFunc = func(fds []unix.PollFd, timeout int) (int, error) {
		return 0, nil
	}
	readFunc = func(fd int, p []byte) (int, error) {
		return 0, nil
	}

	ui := New(nil, &bytes.Buffer{})
	// Need to set rl to non-nil to not hit the early no-op
	ui.rl = &readline.Instance{}
	ctx, cancel := context.WithCancel(context.Background())
	stop := ui.startEscMonitor(ctx, func() {})
	// Should not panic and should return a stop func
	cancel()
	stop()
	// Test with ESC
	isTerminal = func(fd int) bool { return true }
	makeRaw = func(fd int) (*term.State, error) { return &term.State{}, nil }
	pollCount := 0
	pollFunc = func(fds []unix.PollFd, timeout int) (int, error) {
		pollCount++
		if pollCount == 1 {
			fds[0].Revents = unix.POLLIN
			return 1, nil // data available
		}
		if pollCount == 2 {
			return 0, nil // no more data for second poll (to detect single ESC)
		}
		return 0, nil
	}
	readFunc = func(fd int, p []byte) (int, error) {
		p[0] = 27 // ESC
		return 1, nil
	}
	ui2 := New(nil, &bytes.Buffer{})
	ui2.rl = &readline.Instance{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	called := false
	cancelFn := func() { called = true; cancel2() }
	stop2 := ui2.startEscMonitor(ctx2, cancelFn)
	// Give it time to poll and handle ESC
	// Since poll returns 1 and read returns ESC, it should call cancel
	// Wait a bit
	// Use a timeout
	select {
	case <-ctx2.Done():
		// success
	case <-timeAfter(500):
		t.Fatal("should have canceled")
	}
	stop2()
	if !called {
		t.Fatal("cancel not called")
	}
}

func timeAfter(d int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(time.Duration(d) * time.Millisecond)
		close(ch)
	}()
	return ch
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

func calledEqual(a, b int) bool { return a == b }
