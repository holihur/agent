package session

import (
	"context"
	"os"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

func TestEstimateTokens(t *testing.T) {
	msgs := []agent.Message{
		{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: "hello"}}},
	}
	if EstimateTokens(msgs) == 0 {
		t.Fatal("should >0")
	}
}

func TestShouldCompress(t *testing.T) {
	cfg := Config{MaxTokens: 100, Ratio: 0.8, KeepRecent: 2, Compressor: LLMCompressor{Summarize: func(ctx context.Context, prompt string) (string, error) { return "summary", nil }}}
	msgs := make([]agent.Message, 10)
	for i := range msgs {
		msgs[i] = agent.Message{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: string(make([]byte, 50))}}}
	}
	if !ShouldCompress(msgs, cfg) {
		t.Fatal("should compress")
	}
	cfg.Ratio = 1
	if EstimateTokens(msgs) < cfg.Threshold() {
		t.Fatal("should not compress with high threshold")
	}
}

func TestCompressMessages(t *testing.T) {
	cfg := Config{MaxTokens: 100, Ratio: 0.8, KeepRecent: 2, Compressor: LLMCompressor{Summarize: func(ctx context.Context, prompt string) (string, error) { return "summary", nil }}}
	msgs := make([]agent.Message, 5)
	for i := range msgs {
		msgs[i] = agent.Message{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: "msg"}}}
	}
	out, err := CompressMessages(context.Background(), msgs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("expect 3 got %d", len(out))
	}
	if out[0].Blocks[0].Text == "" {
		t.Fatal("summary empty")
	}
}

func TestMaybeCompress(t *testing.T) {
	dir, _ := os.MkdirTemp("", "compress")
	defer os.RemoveAll(dir)
	store := NewFileStore(dir)
	cfg := Config{MaxTokens: 100, Ratio: 0.8, KeepRecent: 2, Compressor: LLMCompressor{Summarize: func(ctx context.Context, prompt string) (string, error) { return "summary", nil }}}
	msgs := make([]agent.Message, 10)
	for i := range msgs {
		msgs[i] = agent.Message{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: string(make([]byte, 50))}}}
	}
	base := "s_a1b2c3d4"
	store.Save(context.Background(), base, msgs)
	newName, newMsgs, ok, err := MaybeCompress(context.Background(), store, base, msgs, cfg)
	if err != nil || !ok {
		t.Fatalf("compress failed %v ok %v", err, ok)
	}
	if newName != "s_a1b2c3d4_1" {
		t.Fatalf("expect s_a1b2c3d4_1 got %s", newName)
	}
	if len(newMsgs) != 3 {
		t.Fatalf("expect 3 got %d", len(newMsgs))
	}
	names, _ := store.Names(context.Background())
	if len(names) != 2 {
		t.Fatalf("expect 2 names got %v", names)
	}
	msgs2 := append(newMsgs, msgs...)
	newName2, _, ok2, err := MaybeCompress(context.Background(), store, newName, msgs2, cfg)
	if err != nil || !ok2 {
		t.Fatalf("second compress failed")
	}
	if newName2 != "s_a1b2c3d4_2" {
		t.Fatalf("expect s_a1b2c3d4_2 got %s", newName2)
	}
}

func TestGenerateSID(t *testing.T) {
	id := GenerateSID()
	if len(id) != 10 || id[:2] != "s_" {
		t.Fatalf("expect s_ prefix 10 len got %q", id)
	}
	id2 := GenerateSID()
	if id == id2 {
		t.Fatal("should be random")
	}
}

func TestNextCompressedName(t *testing.T) {
	if got := NextCompressedName("s_abc123", []string{"s_abc123"}); got != "s_abc123_1" {
		t.Fatalf("got %q", got)
	}
	if got := NextCompressedName("s_abc123_1", []string{"s_abc123", "s_abc123_1"}); got != "s_abc123_2" {
		t.Fatalf("got %q", got)
	}
}

func TestLLMCompressor(t *testing.T) {
	c := LLMCompressor{Summarize: func(ctx context.Context, prompt string) (string, error) {
		return "llm summary", nil
	}}
	msgs := []agent.Message{{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: "hi"}}}}
	s, err := c.Compress(context.Background(), msgs)
	if err != nil || s != "llm summary" {
		t.Fatalf("llm compress failed %v %q", err, s)
	}
}
