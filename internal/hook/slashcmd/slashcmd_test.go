package slashcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/tools"
)

func TestRunSlashPassthrough(t *testing.T) {
	if _, handled := runSlash("3+5 等于几"); handled {
		t.Fatal("plain input must pass through to the LLM")
	}
}

func TestSlashEnabled(t *testing.T) {
	cases := map[string]bool{
		"": true, "on": true, "auto": true,
		"off": false, "none": false,
	}
	for mode, want := range cases {
		if got := enabled(mode); got != want {
			t.Errorf("enabled(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestRunSlashHelp(t *testing.T) {
	out, handled := runSlash("/help")
	if !handled {
		t.Fatal("/help must be handled")
	}
	if !strings.Contains(out, "/help") || !strings.Contains(out, "/exit") {
		t.Fatalf("help must list commands, got %q", out)
	}
	if !strings.Contains(out, "/new") {
		t.Fatalf("help must list /new, got %q", out)
	}
	if !strings.Contains(out, "Ctrl-R") || !strings.Contains(out, "Ctrl-U") {
		t.Fatalf("help must list readline editing keys, got %q", out)
	}
	if !strings.Contains(out, "Ctrl-Z") || !strings.Contains(out, "Alt-D") || !strings.Contains(out, "Ctrl-S") {
		t.Fatalf("help must list Ctrl-Z/Alt-D/Ctrl-S editing keys, got %q", out)
	}
}

func TestRunSlashHelpAlias(t *testing.T) {
	out, handled := runSlash("/h")
	if !handled || out != helpText {
		t.Fatalf("handled=%v out=%q, want help text", handled, out)
	}
}

func TestRunSlashUnknown(t *testing.T) {
	out, handled := runSlash("/foo")
	if !handled || !strings.HasPrefix(out, "unknown command: /foo") {
		t.Fatalf("handled=%v out=%q, want unknown command hint", handled, out)
	}
}

type stubLLM struct{ called bool }

func (s *stubLLM) Turn(context.Context, agent.TurnRequest) (agent.TurnResult, error) {
	s.called = true
	return agent.TurnResult{StopReason: "end_turn"}, nil
}

func TestSlashOffBypassesInterception(t *testing.T) {
	old := *slashCmd
	defer func() { *slashCmd = old }()
	*slashCmd = "off"

	h := agent.NewHooks()
	if err := installSlash(h, hook.Deps{}); err != nil {
		t.Fatalf("installSlash: %v", err)
	}
	stub := &stubLLM{}
	ag := &agent.Agent{LLM: stub, Registry: tools.New(), Hooks: h}
	if _, err := ag.Run(context.Background(), "/help"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stub.called {
		t.Fatal("slashcmd disabled: input must reach the LLM instead of the / command handler")
	}
}
