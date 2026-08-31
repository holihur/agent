package shell

import (
	"context"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/tools"
)

func TestRunShellPassthrough(t *testing.T) {
	if _, handled := runShell("3+5 等于几"); handled {
		t.Fatal("plain input must pass through to the LLM")
	}
}

func TestShellEscapeEnabled(t *testing.T) {
	cases := map[string]bool{
		"": true, "on": true, "auto": true,
		"off": false, "none": false,
	}
	for mode, want := range cases {
		if got := shellEscapeEnabled(mode); got != want {
			t.Errorf("shellEscapeEnabled(%q) = %v, want %v", mode, got, want)
		}
	}
}

type stubLLM struct{ called bool }

func (s *stubLLM) Turn(context.Context, agent.TurnRequest) (agent.TurnResult, error) {
	s.called = true
	return agent.TurnResult{StopReason: "end_turn"}, nil
}

func TestEscapeOffBypassesInterception(t *testing.T) {
	old := *shellEscape
	defer func() { *shellEscape = old }()
	*shellEscape = "off"

	h := agent.NewHooks()
	if err := installShell(h, hook.Deps{}); err != nil {
		t.Fatalf("installShell: %v", err)
	}

	stub := &stubLLM{}
	ag := &agent.Agent{LLM: stub, Registry: tools.New(), Hooks: h}
	if _, err := ag.Run(context.Background(), "!echo off-case"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stub.called {
		t.Fatal("escape disabled: input must reach the LLM instead of the shell escape")
	}

	*shellEscape = "on"
	hooksEnabled := agent.NewHooks()
	if err := installShell(hooksEnabled, hook.Deps{}); err != nil {
		t.Fatalf("installShell: %v", err)
	}
	stubOn := &stubLLM{}
	agOn := &agent.Agent{LLM: stubOn, Registry: tools.New(), Hooks: hooksEnabled}
	answer, err := agOn.Run(context.Background(), "!echo on-case")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stubOn.called {
		t.Fatal("escape enabled: '!echo on-case' must be intercepted, not sent to the LLM")
	}
	if answer != "on-case" {
		t.Fatalf("answer = %q, want %q", answer, "on-case")
	}
}

func TestRunShellEcho(t *testing.T) {
	out, handled := runShell("!echo hook-ok")
	if !handled {
		t.Fatal("! input must be handled")
	}
	if out != "hook-ok" {
		t.Fatalf("out = %q, want %q", out, "hook-ok")
	}
}

func TestRunShellExitStatus(t *testing.T) {
	out, handled := runShell("!false")
	if !handled || !strings.Contains(out, "[exit 1]") {
		t.Fatalf("handled=%v out=%q, want [exit 1]", handled, out)
	}
}

func TestRunShellNoOutput(t *testing.T) {
	out, handled := runShell("!true")
	if !handled || out != "(no output)" {
		t.Fatalf("handled=%v out=%q, want (no output)", handled, out)
	}
}

func TestRunShellEmptyCommand(t *testing.T) {
	out, handled := runShell("!  ")
	if !handled || !strings.HasPrefix(out, "usage:") {
		t.Fatalf("handled=%v out=%q, want usage hint", handled, out)
	}
}
