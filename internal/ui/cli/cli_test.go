package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent/internal/agent"
	"agent/internal/tools"
)

// 非 TTY(管道)路径:readline 不启用,走逐行兜底。
func TestScannerFallbackREPL(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader("hi\n\nexit\n"), &out)
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ui.rl != nil {
		t.Fatal("readline must not start on non-TTY input")
	}
	if !strings.Contains(out.String(), "error: agent not wired") {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "> ") {
		t.Fatalf("prompt missing: %q", out.String())
	}
}

func TestScannerFallbackEOF(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader("hi\n"), &out)
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRespondCollectsFields(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader("octocat\n\n"), &out)
	req := tools.InputRequest{
		Tool: "confirm",
		Prompts: []tools.InputPrompt{{
			Key:     "k1",
			Message: "Proceed?",
			Fields: []tools.InputField{
				{Name: "user", Required: true},
				{Name: "note"},
			},
		}},
	}
	resps, err := ui.Respond(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 1 || resps[0].Key != "k1" {
		t.Fatalf("resps = %+v", resps)
	}
	if resps[0].Content["user"] != "octocat" {
		t.Fatalf("content = %+v", resps[0].Content)
	}
	if _, has := resps[0].Content["note"]; has {
		t.Fatalf("empty optional field must be omitted: %+v", resps[0].Content)
	}
}

func TestConfirmHookDenyByDefault(t *testing.T) {
	ui := New(strings.NewReader("\n"), &bytes.Buffer{})
	confirm := ui.ConfirmHook()
	if d := confirm(agent.ToolCall{Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}); !d.Deny || d.Reason != "user denied" {
		t.Fatalf("empty answer must deny: %+v", d)
	}
}

func TestConfirmHookAcceptsYes(t *testing.T) {
	ui := New(strings.NewReader("y\nyes\n"), &bytes.Buffer{})
	confirm := ui.ConfirmHook()
	if d := confirm(agent.ToolCall{Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}); d.Deny {
		t.Fatalf("y must allow: %+v", d)
	}
	if d := confirm(agent.ToolCall{Name: "shell"}); d.Deny {
		t.Fatalf("yes must allow: %+v", d)
	}
}

func TestRespondInputClosedIsError(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	req := tools.InputRequest{
		Tool:    "t",
		Prompts: []tools.InputPrompt{{Key: "k", Fields: []tools.InputField{{Name: "a", Required: true}}}},
	}
	if _, err := ui.Respond(context.Background(), req); err == nil {
		t.Fatal("expected error when input stream is closed")
	}
}
