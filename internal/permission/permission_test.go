package permission

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

func TestWildcardMatch(t *testing.T) {
	cases := []struct{ pat, s string; want bool }{
		{"*", "a", true},
		{"*", "", true},
		{"a*", "ab", true},
		{"a*", "ba", false},
		{"*b", "ab", true},
		{"a?b", "acb", true},
		{"a?b", "ab", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pat, c.s); got != c.want {
			t.Fatalf("wildcardMatch %q %q got %v want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestMatchRule(t *testing.T) {
	call := agent.ToolCall{Name: "fs__read", Input: json.RawMessage(`{"path":"/tmp/a"}`)}
	if !MatchRule(Rule{Pattern: "fs__*"}, call) {
		t.Fatal("expected match fs__*")
	}
	if !MatchRule(Rule{Pattern: "fs__*", InputPattern: "*/tmp/*"}, call) {
		t.Fatal("expected input match")
	}
	if MatchRule(Rule{Pattern: "fs__*", InputPattern: "*/etc/*"}, call) {
		t.Fatal("should not match /etc")
	}
}

func TestShellPipeline(t *testing.T) {
	if !HasPipeline("cat /tmp/a | cat /etc/passwd") {
		t.Fatal("expect pipeline")
	}
	if HasPipeline(`echo "a|b"`) {
		t.Fatal("quoted pipe should not be pipeline")
	}
	if !HasRiskyShellConstruct("cat /tmp/a; cat /etc/passwd") {
		t.Fatal("expect risky ;")
	}
	if HasRiskyShellConstruct("cat /tmp/a") {
		t.Fatal("single cat should not be risky")
	}
}

func TestStoreAllowAndCheck(t *testing.T) {
	dir, _ := os.MkdirTemp("", "permtest")
	defer os.RemoveAll(dir)
	if err := Allow(dir, "fs__*", "", false); err != nil {
		t.Fatal(err)
	}
	if err := Allow(dir, "shell", "*/tmp/*", false); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolCall{Name: "fs__read", Input: json.RawMessage(`{"path":"/tmp/a"}`)}
	ok, _, _ := Check(dir, call)
	if !ok {
		t.Fatal("expect allowed")
	}
	// shell pipeline without explicit | should be denied even if */tmp/* matches
	call2 := agent.ToolCall{Name: "shell", Input: json.RawMessage(`{"command":"cat /tmp/a | cat /etc/passwd"}`)}
	rule := Rule{Pattern: "shell", InputPattern: "*/tmp/*"}
	if MatchRule(rule, call2) {
		t.Fatal("pipeline should be denied without | in pattern")
	}
	rule2 := Rule{Pattern: "shell", InputPattern: "*|*"}
	if !MatchRule(rule2, call2) {
		t.Fatal("pipeline with | pattern should allow")
	}
	if err := Deny(dir, "fs__write", "", false); err != nil {
		t.Fatal(err)
	}
	_, denied, _, _ := CheckWithDeny(dir, agent.ToolCall{Name: "fs__write"})
	if !denied {
		t.Fatal("expect denied")
	}
}

func TestLoadAndList(t *testing.T) {
	dir, _ := os.MkdirTemp("", "permtest2")
	defer os.RemoveAll(dir)
	_ = Allow(dir, "a", "", false)
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Allow) == 0 {
		t.Fatal("expect allow")
	}
	if _, err := List(dir); err != nil {
		t.Fatal(err)
	}
}
