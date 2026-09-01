package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

// allBlockTypes 构造覆盖四种块类型的历史:text / tool_use(含 Input)/
// tool_result(含 is_error)/ thinking(含签名 —— 回填历史必须原样往返)。
func allBlockTypes() []agent.Message {
	return []agent.Message{
		{Role: agent.RoleUser, Blocks: []agent.Block{
			agent.NewText("列出文件"),
		}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{
			{Type: agent.BlockThinking, Text: "先看目录", Signature: "sig-abc=="},
			agent.NewToolUse("tu_1", "shell", json.RawMessage(`{"command":"ls"}`)),
		}},
		{Role: agent.RoleUser, Blocks: []agent.Block{
			agent.NewToolResult("tu_1", "main.go\nREADME.md", false),
		}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{
			agent.NewText("有 2 个文件"),
			{Type: agent.BlockToolResult, ToolUseID: "tu_2", Content: "boom", IsError: true},
		}},
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := NewFileStore(t.TempDir())
	want := allBlockTypes()
	if err := s.Save(context.Background(), "rt", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(context.Background(), "rt")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestFileStoreOverwrite(t *testing.T) {
	s := NewFileStore(t.TempDir())
	ctx := context.Background()
	if err := s.Save(ctx, "s", allBlockTypes()); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	v2 := []agent.Message{{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("v2")}}}
	if err := s.Save(ctx, "s", v2); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	got, err := s.Load(ctx, "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, v2) {
		t.Fatalf("stale content after overwrite: %+v", got)
	}
}

func TestFileStoreNamesAndDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	ctx := context.Background()
	for _, name := range []string{"b", "a"} {
		if err := s.Save(ctx, name, nil); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), nil, 0o644); err != nil {
		t.Fatalf("seed non-session file: %v", err)
	}
	names, err := s.Names(ctx)
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names = %v, want [a b]", names)
	}
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "a"); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("delete missing err = %v, want ErrSessionNotFound", err)
	}
}

func TestFileStoreNotFound(t *testing.T) {
	s := NewFileStore(t.TempDir())
	if _, err := s.Load(context.Background(), "nope"); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("load missing err = %v, want ErrSessionNotFound", err)
	}
}

func TestFileStoreInvalidName(t *testing.T) {
	s := NewFileStore(t.TempDir())
	ctx := context.Background()
	tooLong := strings.Repeat("x", 65)
	for _, name := range []string{"", "../escape", "a/b", "with.dot", tooLong} {
		if err := s.Save(ctx, name, nil); err == nil {
			t.Fatalf("Save(%q) must fail", name)
		}
		if _, err := s.Load(ctx, name); err == nil {
			t.Fatalf("Load(%q) must fail", name)
		}
		if err := s.Delete(ctx, name); err == nil {
			t.Fatalf("Delete(%q) must fail", name)
		}
	}
}

func TestFileStoreMalformedLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.jsonl"), []byte("{not json}\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := NewFileStore(dir).Load(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("err = %v, want line 1 decode failure", err)
	}
}

func TestFileStoreSaveEncodeError(t *testing.T) {
	s := NewFileStore(t.TempDir())
	bad := []agent.Message{{Role: agent.RoleAssistant, Blocks: []agent.Block{
		agent.NewToolUse("tu_1", "x", json.RawMessage("{invalid")),
	}}}
	if err := s.Save(context.Background(), "bad", bad); err == nil || !strings.Contains(err.Error(), "encode") {
		t.Fatalf("err = %v, want encode failure", err)
	}
}

func TestFileStoreDirIsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(dir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewFileStore(dir)
	ctx := context.Background()
	if err := s.Save(ctx, "x", nil); err == nil {
		t.Fatal("Save must fail when dir is a file")
	}
	if _, err := s.Load(ctx, "x"); err == nil || errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("Load err = %v, want non-notexist failure", err)
	}
	if _, err := s.Names(ctx); err == nil || errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("Names err = %v, want non-notexist failure", err)
	}
	if err := s.Delete(ctx, "x"); err == nil {
		t.Fatal("Delete must fail when dir is a file")
	}
}

func TestFileStoreSaveWriteAndRenameErrors(t *testing.T) {
	base := t.TempDir()

	ro := filepath.Join(base, "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if err := NewFileStore(ro).Save(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("err = %v, want write failure on read-only dir", err)
	}

	weird := filepath.Join(base, "weird")
	s := NewFileStore(weird)
	if err := os.MkdirAll(filepath.Join(weird, "x.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("err = %v, want rename failure", err)
	}
}

func TestFileStoreOversizedLine(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("x", 17<<20) // 超过 Load 的 16MB 单行上限
	if err := os.WriteFile(filepath.Join(dir, "big.jsonl"), []byte(huge+"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := NewFileStore(dir).Load(context.Background(), "big"); err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("err = %v, want scanner failure", err)
	}
}

func TestNextName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := NewFileStore(dir)

	// 空 store:work → work-2
	got, err := NextName(ctx, store, "work")
	if err != nil || got != "work-2" {
		t.Fatalf("NextName(work) = %q, %v; want work-2", got, err)
	}

	// 落盘 work 与 work-2 后:work → work-3
	for _, n := range []string{"work", "work-2"} {
		if err := store.Save(ctx, n, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err = NextName(ctx, store, "work")
	if err != nil || got != "work-3" {
		t.Fatalf("NextName(work) = %q, %v; want work-3", got, err)
	}

	// base 自带计数:work-2 → 顺着 work-4(work-3 未占用则取 work-3)
	if err := store.Save(ctx, "work-3", nil); err != nil {
		t.Fatal(err)
	}
	got, err = NextName(ctx, store, "work-2")
	if err != nil || got != "work-4" {
		t.Fatalf("NextName(work-2) = %q, %v; want work-4", got, err)
	}

	// 无计数后缀的怪名:数字后缀不合法时按 1 处理
	got, err = NextName(ctx, store, "weird")
	if err != nil || got != "weird-2" {
		t.Fatalf("NextName(weird) = %q, %v; want weird-2", got, err)
	}
}
