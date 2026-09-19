package main

import (
	"os"
	"reflect"
	"testing"

	"github.com/holihur/agent/internal/mcp"
)

func TestMCPFlagsString(t *testing.T) {
	var f mcpFlags
	if s := f.String(); s != "" {
		t.Fatalf("empty String = %q", s)
	}
	f = mcpFlags{{Name: "a", Command: "cmd", Args: []string{"arg1", "arg2"}}}
	want := "a=cmd arg1 arg2"
	if s := f.String(); s != want {
		t.Fatalf("String = %q, want %q", s, want)
	}
	fNil := (*mcpFlags)(nil)
	if s := fNil.String(); s != "" {
		t.Fatalf("nil String = %q", s)
	}
}

func TestMCPFlagsSetErrors(t *testing.T) {
	var f mcpFlags
	cases := []struct {
		input string
		isErr bool
	}{
		{"", true},
		{"=", true},
		{"name=", true},
		{"bad name=cmd", true},
		{"tool", true},
		{"tool=", true},
		{"unknown=   ", true},
		{"a=cmd arg", false},
		{"b=https://example.com/mcp", false},
		{"c=http://example.com/mcp", false},
	}
	for _, c := range cases {
		err := f.Set(c.input)
		if (err != nil) != c.isErr {
			t.Fatalf("Set(%q) err=%v want err=%v", c.input, err, c.isErr)
		}
	}
}

func TestIsHTTPURL(t *testing.T) {
	if !isHTTPURL("http://a") || !isHTTPURL("https://a") {
		t.Fatal("should be true")
	}
	if isHTTPURL("ftp://a") || isHTTPURL("a") || isHTTPURL("") {
		t.Fatal("should be false")
	}
}

func TestEnvFirst(t *testing.T) {
	t.Setenv("TEST_ENV_FIRST_A", "a")
	t.Setenv("TEST_ENV_FIRST_B", "b")
	if got := envFirst("NOPE", "TEST_ENV_FIRST_A", "TEST_ENV_FIRST_B"); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := envFirst("TEST_ENV_FIRST_B", "TEST_ENV_FIRST_A"); got != "b" {
		t.Fatalf("got %q", got)
	}
	if got := envFirst("NOPE1", "NOPE2"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMergeMCPServers_FileAndFlagOverlap(t *testing.T) {
	fromFile := []mcp.JSONServer{
		{Name: "dup", Command: "old"},
		{Name: "keep", Command: "keep"},
	}
	fromFlags := mcpFlags{{Name: "dup", Command: "new"}}
	specs := mergeMCPServers(fromFile, fromFlags)
	if len(specs) != 2 {
		t.Fatalf("len = %d", len(specs))
	}
	// dup should be overridden to new, keep should remain, order preserved
	foundDup := false
	for _, s := range specs {
		if s.Name == "dup" && s.Command == "new" {
			foundDup = true
		}
	}
	if !foundDup {
		t.Fatalf("dup not overridden: %+v", specs)
	}
}

func TestEnvPairsSorted(t *testing.T) {
	got := envPairs(map[string]string{"Z": "1", "A": "2"})
	if !reflect.DeepEqual(got, []string{"A=2", "Z=1"}) {
		t.Fatalf("got %v", got)
	}
	if got := envPairs(nil); got != nil {
		t.Fatalf("nil should be nil, got %v", got)
	}
}

// TestEnvOrFileFirst 覆盖安全评审 §3.4：密钥优先从环境读取，否则从 0600
// 临时文件（NAME_FILE）读取，避免密钥出现在子进程环境。
func TestEnvOrFileFirst(t *testing.T) {
	dir := t.TempDir()
	keyFile := dir + "/key"
	if err := os.WriteFile(keyFile, []byte("  sk-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_KEY_FILE", keyFile)
	t.Setenv("TEST_KEY", "")
	if got := envOrFileFirst("TEST_KEY"); got != "sk-from-file" {
		t.Fatalf("file key = %q", got)
	}
	t.Setenv("TEST_KEY", "sk-from-env")
	if got := envOrFileFirst("TEST_KEY"); got != "sk-from-env" {
		t.Fatalf("env key = %q", got)
	}
	if got := envOrFileFirst("MISSING_KEY"); got != "" {
		t.Fatalf("missing key = %q", got)
	}
}
