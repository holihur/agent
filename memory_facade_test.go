package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	core "github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/memory"
	"github.com/holihur/agent/internal/tools"
)

// fakeMemory 是门面测试用的最小 Memory 实现。
type fakeMemory struct{ keys []string }

func (f *fakeMemory) Put(context.Context, string, string) error { return nil }
func (f *fakeMemory) Get(_ context.Context, key string) (string, error) {
	for _, k := range f.keys {
		if k == key {
			return "v", nil
		}
	}
	return "", fmt.Errorf("%w: %s", core.ErrMemoryNotFound, key)
}
func (f *fakeMemory) Search(_ context.Context, q string) ([]string, error) { return f.keys, nil }
func (f *fakeMemory) Keys(context.Context) ([]string, error)               { return f.keys, nil }
func (f *fakeMemory) Delete(context.Context, string) error                 { return nil }

func TestMemoryFileTree(t *testing.T) {
	out := memory.Tree([]string{"lang-go", "lang-rs", "pet"})
	want := ".agent/memory/\n  lang/go.json\n  lang/rs.json\npet.json\n"
	if out != want {
		t.Fatalf("tree = %q, want %q", out, want)
	}
}

func TestMemoryFileTreeTruncates(t *testing.T) {
	keys := make([]string, memory.MaxTreeKeys+3)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%03d", i)
	}
	out := memory.Tree(keys)
	if !strings.Contains(out, fmt.Sprintf("(+%d more, %d total)", 3, len(keys))) {
		t.Fatalf("tree missing truncation note: %q", out)
	}
	if strings.Count(out, "\n") != memory.MaxTreeKeys+2 { // 头行 + 叶子 + 截断行
		t.Fatalf("tree line count = %d", strings.Count(out, "\n"))
	}
}

// captureLLM 是捕获 TurnRequest 的桩 LLM。
type captureLLM struct{ system string }

func (c *captureLLM) Turn(_ context.Context, r core.TurnRequest) (core.TurnResult, error) {
	c.system = r.System
	return core.TurnResult{StopReason: "end_turn"}, nil
}

func TestMemoryPromptInjection(t *testing.T) {
	a := &Agent{inner: &core.Agent{System: "base", Hooks: core.NewHooks(), Registry: tools.New()}}
	a.registerMemoryPrompt(&fakeMemory{keys: []string{"lang-go", "pet"}})
	var llm captureLLM
	a.inner.LLM = &llm
	if _, err := a.inner.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(llm.system, "base") || !strings.Contains(llm.system, "lang/go.json") || !strings.Contains(llm.system, "pet.json") {
		t.Fatalf("injected system = %q", llm.system)
	}
}
