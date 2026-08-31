package hook

import (
	"strings"
	"testing"
)

func TestRunShellPassthrough(t *testing.T) {
	if _, handled := runShell("3+5 等于几"); handled {
		t.Fatal("plain input must pass through to the LLM")
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
