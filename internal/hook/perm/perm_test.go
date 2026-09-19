package perm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

// TestCheckDeniedFailsClosed 覆盖安全评审 §L11：权限规则损坏/不可读时不得静默
// 放行（fail-closed）。
func TestCheckDeniedFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离全局权限文件
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".agent", "permissions.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	denied, reason := checkDenied(cwd, agent.ToolCall{Name: "shell"})
	if !denied {
		t.Fatal("corrupt rules must deny (fail-closed)")
	}
	if !strings.Contains(reason, "unavailable") {
		t.Fatalf("reason = %q", reason)
	}
}

// TestCheckDeniedNoRules 无规则文件时应正常放行。
func TestCheckDeniedNoRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if denied, _ := checkDenied(t.TempDir(), agent.ToolCall{Name: "shell"}); denied {
		t.Fatal("no rules should not deny")
	}
}
