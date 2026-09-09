package agent

// 记忆抽象层的黑盒测试:只经公开 API(New/Config/门面方法)观察行为,
// 不触碰 inner/registry 等包内字段。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/memory"
)

// newMemoryAgent 构造带文件记忆的 agent(env 提供凭据)。
func newMemoryAgent(t *testing.T) *Agent {
	t.Helper()
	envCreds(t)
	a, err := New(Config{Memory: memory.NewFileStore(filepath.Join(t.TempDir(), "memory"))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestMemoryToolsExposedToModel:配置 Memory 后模型应看到三个记忆工具。
func TestMemoryToolsExposedToModel(t *testing.T) {
	a := newMemoryAgent(t)
	names, err := a.ToolNames(context.Background())
	if err != nil {
		t.Fatalf("ToolNames: %v", err)
	}
	for _, want := range []string{"memory_save", "memory_search", "memory_forget"} {
		if !contains(names, want) {
			t.Fatalf("tools = %v, missing %q", names, want)
		}
	}
}

// TestMemoryFacadeRoundtrip:Remember → Recall/SearchMemory/MemoryKeys → Forget 全链路。
func TestMemoryFacadeRoundtrip(t *testing.T) {
	a := newMemoryAgent(t)
	ctx := context.Background()
	if err := a.Remember(ctx, "user-fav", "likes go"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := a.Remember(ctx, "pet", "cat"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if v, err := a.Recall(ctx, "user-fav"); err != nil || v != "likes go" {
		t.Fatalf("Recall = %q, %v", v, err)
	}
	if keys, _ := a.MemoryKeys(ctx); len(keys) != 2 || keys[0] != "pet" {
		t.Fatalf("MemoryKeys = %v", keys)
	}
	if hits, _ := a.SearchMemory(ctx, "GO"); len(hits) != 1 || hits[0] != "user-fav" {
		t.Fatalf("SearchMemory = %v", hits)
	}
	if err := a.Forget(ctx, "pet"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := a.Recall(ctx, "pet"); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("Recall err = %v, want ErrMemoryNotFound", err)
	}
}

// TestMemoryPersistAcrossAgents:文件存储跨实例持久;第二个 agent 应看到同一份记忆。
func TestMemoryPersistAcrossAgents(t *testing.T) {
	envCreds(t)
	dir := filepath.Join(t.TempDir(), "memory")
	a1, err := New(Config{Memory: memory.NewFileStore(dir)})
	if err != nil {
		t.Fatalf("New a1: %v", err)
	}
	if err := a1.Remember(context.Background(), "note", "hello"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	_ = a1.Close()

	a2, err := New(Config{Memory: memory.NewFileStore(dir)})
	if err != nil {
		t.Fatalf("New a2: %v", err)
	}
	defer a2.Close()
	if v, err := a2.Recall(context.Background(), "note"); err != nil || v != "hello" {
		t.Fatalf("a2 Recall = %q, %v", v, err)
	}
}

// TestMemoryNotConfigured:未配置 Memory 时门面方法一律报错。
func TestMemoryNotConfigured(t *testing.T) {
	envCreds(t)
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	ctx := context.Background()
	if err := a.Remember(ctx, "k", "v"); err == nil {
		t.Fatal("Remember: want error")
	}
	if _, err := a.Recall(ctx, "k"); err == nil {
		t.Fatal("Recall: want error")
	}
	if _, err := a.SearchMemory(ctx, "q"); err == nil {
		t.Fatal("SearchMemory: want error")
	}
	if _, err := a.MemoryKeys(ctx); err == nil {
		t.Fatal("MemoryKeys: want error")
	}
	if err := a.Forget(ctx, "k"); err == nil {
		t.Fatal("Forget: want error")
	}
}

// TestMemoryPromptViaRun:黑盒走 Run 路径,断言记忆索引出现在出站 system prompt。
func TestMemoryPromptViaRun(t *testing.T) {
	a := newMemoryAgent(t)
	if err := a.Remember(context.Background(), "lang-go", "go"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	llm := &stubLLM{resp: core.TurnResult{StopReason: "end_turn"}}
	a.inner.LLM = llm
	if _, err := a.inner.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sys := llm.got.System
	if !strings.Contains(sys, "lang/go.json") {
		t.Fatalf("system prompt missing memory tree: %q", sys)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
