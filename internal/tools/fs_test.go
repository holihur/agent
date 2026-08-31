package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// must 以 JSON 字面量构建入参并调用工具;断言无 error 并返回结果文本。
func mustCall(t *testing.T, fn func(context.Context, json.RawMessage) (string, error), raw string) string {
	t.Helper()
	out, err := fn(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("tool call failed: %v\ninput: %s", err, raw)
	}
	return out
}

// mustErr 断言工具调用返回 error,并返回错误消息。
func mustErr(t *testing.T, fn func(context.Context, json.RawMessage) (string, error), raw string) string {
	t.Helper()
	_, err := fn(context.Background(), json.RawMessage(raw))
	if err == nil {
		t.Fatalf("expected error, got success\ninput: %s", raw)
	}
	return err.Error()
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---- read ----

func TestReadBatchAndRanges(t *testing.T) {
	a := writeTemp(t, "a.txt", "l1\nl2\nl3\nl4\nl5\n")
	b := writeTemp(t, "b.txt", "x\ny\n")
	pathA, pathB := filepath.ToSlash(a), filepath.ToSlash(b)

	out := mustCall(t, toolRead, `{"paths":["`+pathA+`","`+pathB+`"]}`)
	if !strings.Contains(out, pathA) || !strings.Contains(out, "lines 1-5") {
		t.Fatalf("batch read missing full file info:\n%s", out)
	}
	if !strings.Contains(out, "l1") || !strings.Contains(out, "l5") {
		t.Fatalf("batch read missing content:\n%s", out)
	}
	if !strings.Contains(out, pathB) || !strings.Contains(out, "lines 1-2") || !strings.Contains(out, "y") {
		t.Fatalf("batch read missing second file:\n%s", out)
	}
	if !strings.Contains(out, "2/2 files read") {
		t.Fatalf("missing summary:\n%s", out)
	}

	// offset/limit 分段:读 a.txt 第 3-4 行。
	out = mustCall(t, toolRead, `{"paths":["`+pathA+`"],"offset":3,"limit":2}`)
	if !strings.Contains(out, "lines 3-4") || !strings.Contains(out, "l3") || strings.Contains(out, "l1\n") {
		t.Fatalf("offset/limit read wrong:\n%s", out)
	}
}

func TestReadIndependentFailure(t *testing.T) {
	a := writeTemp(t, "a.txt", "hello\n")
	out := mustCall(t, toolRead, `{"paths":["`+filepath.ToSlash(a)+`","/nonexistent/x.txt"]}`)
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, "1/2 files read") {
		t.Fatalf("failed file should be reported independently:\n%s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("good file content should still be present:\n%s", out)
	}
}

func TestReadValidation(t *testing.T) {
	msg := mustErr(t, toolRead, `{"paths":[]}`)
	if !strings.Contains(msg, "paths") {
		t.Fatalf("unexpected error: %s", msg)
	}
	msg = mustErr(t, toolRead, `{"offset":-1,"paths":["x"]}`)
	if !strings.Contains(msg, "offset") {
		t.Fatalf("unexpected error: %s", msg)
	}
}

func TestReadChunkBoundary(t *testing.T) {
	// 超过 maxReadChunk 行:结果提示续读,不静默截断。
	var sb strings.Builder
	for i := 1; i <= maxReadChunk+5; i++ {
		sb.WriteString("line\n")
	}
	path := writeTemp(t, "big.txt", sb.String())
	out := mustCall(t, toolRead, `{"paths":["`+filepath.ToSlash(path)+`"]}`)
	if !strings.Contains(out, "stopped at line") || !strings.Contains(out, "offset=4001") {
		t.Fatalf("chunk truncation should hint continuation:\n%s", out)
	}
}

// ---- write ----

func TestWriteBatchCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	out := mustCall(t, toolWrite, `{"files":[
		{"path":"`+filepath.ToSlash(filepath.Join(dir, "new", "a.go"))+`","content":"package a"},
		{"path":"`+filepath.ToSlash(filepath.Join(dir, "b.txt"))+`","content":"hi"}
	]}`)
	if !strings.Contains(out, "wrote 9 bytes") || !strings.Contains(out, "wrote 2 bytes") {
		t.Fatalf("write report wrong:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "new", "a.go"))
	if err != nil || string(data) != "package a" {
		t.Fatalf("file not written correctly: %v %q", err, data)
	}
}

func TestWriteOverwrite(t *testing.T) {
	path := writeTemp(t, "f.txt", "old")
	mustCall(t, toolWrite, `{"files":[{"path":"`+filepath.ToSlash(path)+`","content":"new"}]}`)
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Fatalf("overwrite failed: %q", data)
	}
}

func TestWriteValidation(t *testing.T) {
	dir := t.TempDir()
	msg := mustErr(t, toolWrite, `{"files":[{"path":"`+filepath.ToSlash(dir)+`","content":"x"}]}`)
	if !strings.Contains(msg, "directory") {
		t.Fatalf("unexpected error: %s", msg)
	}
	msg = mustErr(t, toolWrite, `{"files":[]}`)
	if !strings.Contains(msg, "files") {
		t.Fatalf("unexpected error: %s", msg)
	}
}

// ---- edit ----

func TestEditBatchAndOrdering(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("aaa\nbbb\nccc\n"), 0o644)
	os.WriteFile(b, []byte("one two\n"), 0o644)

	out := mustCall(t, toolEdit, `{"edits":[
		{"path":"`+filepath.ToSlash(a)+`","oldText":"bbb","newText":"BBB"},
		{"path":"`+filepath.ToSlash(b)+`","oldText":"one","newText":"uno"},
		{"path":"`+filepath.ToSlash(a)+`","oldText":"ccc","newText":"CCC"}
	]}`)
	if !strings.Contains(out, "2 edit(s) applied") || !strings.Contains(out, "1 edit(s) applied") {
		t.Fatalf("edit report wrong:\n%s", out)
	}
	data, _ := os.ReadFile(a)
	if string(data) != "aaa\nBBB\nCCC\n" {
		t.Fatalf("a.txt edits wrong: %q", data)
	}
	data, _ = os.ReadFile(b)
	if string(data) != "uno two\n" {
		t.Fatalf("b.txt edits wrong: %q", data)
	}
}

func TestEditChainedWithinFile(t *testing.T) {
	// 同一文件多条 edit 按数组顺序应用:第二条的 oldText 匹配第一条的产物。
	path := writeTemp(t, "c.txt", "x\n")
	mustCall(t, toolEdit, `{"edits":[
		{"path":"`+filepath.ToSlash(path)+`","oldText":"x","newText":"y"},
		{"path":"`+filepath.ToSlash(path)+`","oldText":"y","newText":"z"}
	]}`)
	data, _ := os.ReadFile(path)
	if string(data) != "z\n" {
		t.Fatalf("chained edits wrong: %q", data)
	}
}

func TestEditAtomicOnFailure(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("keep\n"), 0o644)
	os.WriteFile(b, []byte("target\n"), 0o644)

	// b 的 oldText 不存在 → 整批失败,a 不得被修改(原子性)。
	msg := mustErr(t, toolEdit, `{"edits":[
		{"path":"`+filepath.ToSlash(a)+`","oldText":"keep","newText":"CHANGED"},
		{"path":"`+filepath.ToSlash(b)+`","oldText":"ghost","newText":"x"}
	]}`)
	if !strings.Contains(msg, "not found") {
		t.Fatalf("unexpected error: %s", msg)
	}
	data, _ := os.ReadFile(a)
	if string(data) != "keep\n" {
		t.Fatalf("atomicity violated: a.txt modified despite failure: %q", data)
	}

	// oldText 不唯一(出现 2 次)同样整批失败。
	os.WriteFile(a, []byte("dup dup\n"), 0o644)
	msg = mustErr(t, toolEdit, `{"edits":[{"path":"`+filepath.ToSlash(a)+`","oldText":"dup","newText":"x"}]}`)
	if !strings.Contains(msg, "not unique") {
		t.Fatalf("unexpected error: %s", msg)
	}
	data, _ = os.ReadFile(a)
	if string(data) != "dup dup\n" {
		t.Fatalf("atomicity violated on ambiguity: %q", data)
	}
}

func TestEditValidation(t *testing.T) {
	path := writeTemp(t, "d.txt", "x\n")
	msg := mustErr(t, toolEdit, `{"edits":[{"path":"`+filepath.ToSlash(path)+`","oldText":"","newText":"y"}]}`)
	if !strings.Contains(msg, "empty oldText") {
		t.Fatalf("unexpected error: %s", msg)
	}
	msg = mustErr(t, toolEdit, `{"edits":[]}`)
	if !strings.Contains(msg, "edits") {
		t.Fatalf("unexpected error: %s", msg)
	}
}

func TestNewBuiltinIncludesFS(t *testing.T) {
	p, err := NewBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	defs, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, want := range []string{"shell", "read", "write", "edit"} {
		if !names[want] {
			t.Fatalf("NewBuiltin missing tool %q (have %v)", want, names)
		}
	}
}
