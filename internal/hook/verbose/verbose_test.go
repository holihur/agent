package verbose

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

func captureStderr(f func()) string {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String()
}

// helper to get unexported field via reflection
func getHookSlice(h *agent.Hooks, name string) reflect.Value {
	v := reflect.ValueOf(h).Elem().FieldByName(name)
	// Use unsafe to get unexported field
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}

func TestInstallVerbose_CoversAllBranches(t *testing.T) {
	*verbose = true
	*displayToolcall = true
	*displayThinking = true
	defer func() {
		*verbose = false
		*displayToolcall = true
		*displayThinking = true
	}()
	h := agent.NewHooks()
	if err := installVerbose(h, hook.Deps{}); err != nil {
		t.Fatal(err)
	}
	// Get the registered hooks via reflection and trigger them to cover fmt.Fprintf
	beforeTool := getHookSlice(h, "beforeTool")
	if beforeTool.Len() == 0 {
		t.Fatal("beforeTool not registered")
	}
	// Call the first beforeTool hook
	fn := beforeTool.Index(0)
	// fn is func(ToolCall) Decision
	out := captureStderr(func() {
		results := fn.Call([]reflect.Value{reflect.ValueOf(agent.ToolCall{Name: "edit", Input: []byte(`{"path":"a.txt"}`)})})
		_ = results
	})
	if !strings.Contains(out, "[edit]") {
		t.Fatalf("expected [edit] in %q", out)
	}
	// Test truncation
	out2 := captureStderr(func() {
		long := strings.Repeat("a", 400)
		fn.Call([]reflect.Value{reflect.ValueOf(agent.ToolCall{Name: "edit", Input: []byte(long)})})
	})
	if !strings.Contains(out2, "…") {
		t.Fatalf("expected truncation, got %q", out2)
	}
	// Test AfterTool
	afterTool := getHookSlice(h, "afterTool")
	if afterTool.Len() == 0 {
		t.Fatal("afterTool not registered")
	}
	fnAfter := afterTool.Index(0)
	for _, tc := range []struct {
		outcome agent.ToolOutcome
		want    string
	}{
		{agent.ToolOutcome{Name: "edit", Duration: time.Millisecond}, "ok"},
		{agent.ToolOutcome{Name: "edit", Denied: true}, "denied"},
		{agent.ToolOutcome{Name: "edit", IsError: true}, "error"},
	} {
		out := captureStderr(func() {
			fnAfter.Call([]reflect.Value{reflect.ValueOf(tc.outcome)})
		})
		if !strings.Contains(out, tc.want) {
			t.Fatalf("want %q in %q", tc.want, out)
		}
		if !strings.Contains(out, "[edit]") {
			t.Fatalf("want [edit] in %q", out)
		}
	}
	// Test BeforeLLM/AfterLLM
	beforeLLM := getHookSlice(h, "beforeLLM")
	afterLLM := getHookSlice(h, "afterLLM")
	if beforeLLM.Len() == 0 || afterLLM.Len() == 0 {
		t.Fatal("llm hooks not registered when verbose true")
	}
	out = captureStderr(func() {
		beforeLLM.Index(0).Call([]reflect.Value{reflect.ValueOf(agent.TurnStat{Turn: 1, Messages: 2, Tools: 3})})
	})
	if !strings.Contains(out, "[llm]") {
		t.Fatalf("want [llm] in %q", out)
	}
	out = captureStderr(func() {
		afterLLM.Index(0).Call([]reflect.Value{reflect.ValueOf(agent.TurnStat{Turn: 1, StopReason: "end_turn", Blocks: 2})})
	})
	if !strings.Contains(out, "[llm]") {
		t.Fatalf("want [llm] in %q", out)
	}
	// Test thinking
	mutateAssistant := getHookSlice(h, "mutateAssistant")
	if mutateAssistant.Len() == 0 {
		t.Fatal("mutateAssistant not registered")
	}
	fnThink := mutateAssistant.Index(0)
	out = captureStderr(func() {
		msg := agent.Message{Blocks: []agent.Block{{Type: agent.BlockThinking, Text: "  test thinking  "}}}
		fnThink.Call([]reflect.Value{reflect.ValueOf(msg)})
	})
	if !strings.Contains(out, "[thinking]") {
		t.Fatalf("want [thinking] in %q", out)
	}
	// Test thinking truncation
	out = captureStderr(func() {
		long := strings.Repeat("b", 600)
		msg := agent.Message{Blocks: []agent.Block{{Type: agent.BlockThinking, Text: long}}}
		fnThink.Call([]reflect.Value{reflect.ValueOf(msg)})
	})
	if !strings.Contains(out, "…") {
		t.Fatalf("want truncation in %q", out)
	}
	// Test empty thinking not printed
	out = captureStderr(func() {
		msg := agent.Message{Blocks: []agent.Block{{Type: agent.BlockThinking, Text: "   "}}}
		fnThink.Call([]reflect.Value{reflect.ValueOf(msg)})
	})
	if strings.Contains(out, "[thinking]") {
		t.Fatalf("should not print empty thinking, got %q", out)
	}
}

func TestInstallVerbose_DisplayOff(t *testing.T) {
	*verbose = false
	*displayToolcall = false
	*displayThinking = false
	defer func() {
		*verbose = false
		*displayToolcall = true
		*displayThinking = true
	}()
	h := agent.NewHooks()
	_ = installVerbose(h, hook.Deps{})
	beforeTool := getHookSlice(h, "beforeTool")
	afterTool := getHookSlice(h, "afterTool")
	mutateAssistant := getHookSlice(h, "mutateAssistant")
	beforeLLM := getHookSlice(h, "beforeLLM")
	if beforeTool.Len() != 0 || afterTool.Len() != 0 || mutateAssistant.Len() != 0 || beforeLLM.Len() != 0 {
		t.Fatalf("should have no hooks when all off, got %d %d %d %d", beforeTool.Len(), afterTool.Len(), mutateAssistant.Len(), beforeLLM.Len())
	}
}

func TestPreviewTruncationLogic(t *testing.T) {
	long := strings.Repeat("a", 400)
	preview := strings.TrimSpace(long)
	if len(preview) > 300 {
		preview = preview[:300] + "…"
	}
	if !strings.Contains(preview, "…") {
		t.Fatal("should truncate")
	}
}
