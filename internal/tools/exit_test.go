package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRegisterExit_RequestsAndReports(t *testing.T) {
	p := NewLocal()
	flag := &ExitFlag{}
	if err := RegisterExit(p, flag); err != nil {
		t.Fatal(err)
	}
	if flag.Requested() {
		t.Fatal("flag should start unset")
	}
	res, err := p.CallTool(context.Background(), "exit", json.RawMessage(`{"reason":"task done"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !flag.Requested() {
		t.Fatal("exit tool should set flag")
	}
	if !strings.Contains(res.Text, "task done") {
		t.Fatalf("result text = %q, want reason echoed", res.Text)
	}
}

func TestRegisterExit_EmptyInput(t *testing.T) {
	p := NewLocal()
	flag := &ExitFlag{}
	if err := RegisterExit(p, flag); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CallTool(context.Background(), "exit", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !flag.Requested() {
		t.Fatal("flag should be set")
	}
}
