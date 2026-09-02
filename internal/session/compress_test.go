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
	cfg := Config{MaxTokens: 100, Ratio: 0.8, KeepRecent: 2, Compressor: SimpleCompressor{}}
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
	cfg := Config{MaxTokens: 100, Ratio: 0.8, KeepRecent: 2, Compressor: SimpleCompressor{}}
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
	cfg := Config{MaxTokens: 100, Ratio: 0.8, KeepRecent: 2, Compressor: SimpleCompressor{}}
	msgs := make([]agent.Message, 10)
	for i := range msgs {
		msgs[i] = agent.Message{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: string(make([]byte, 50))}}}
	}
	store.Save(context.Background(), "dev", msgs)
	newName, newMsgs, ok, err := MaybeCompress(context.Background(), store, "dev", msgs, cfg)
	if err != nil || !ok {
		t.Fatalf("compress failed %v ok %v", err, ok)
	}
	if newName != "dev_1" {
		t.Fatalf("expect dev_1 got %s", newName)
	}
	if len(newMsgs) != 3 {
		t.Fatalf("expect 3 got %d", len(newMsgs))
	}
	names, _ := store.Names(context.Background())
	if len(names) != 2 {
		t.Fatalf("expect 2 names got %v", names)
	}
	// second compress dev_1 -> dev_2
	msgs2 := append(newMsgs, msgs...)
	newName2, _, ok2, err := MaybeCompress(context.Background(), store, newName, msgs2, cfg)
	if err != nil || !ok2 {
		t.Fatalf("second compress failed")
	}
	if newName2 != "dev_2" {
		t.Fatalf("expect dev_2 got %s", newName2)
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
