package session

import (
	"context"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

// TestLLMCompressorDefaultTemplate 黑盒验证内嵌 md 模板的渲染输出。
func TestLLMCompressorDefaultTemplate(t *testing.T) {
	var got string
	c := LLMCompressor{Summarize: func(_ context.Context, prompt string) (string, error) {
		got = prompt
		return "summary", nil
	}}
	msgs := []agent.Message{
		{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: "hi"}}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockText, Text: "hello"}}},
	}
	if _, err := c.Compress(context.Background(), msgs); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	for _, want := range []string{"Summarize", "<user>\nhi\n</user>", "<assistant>\nhello\n</assistant>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

// TestLLMCompressorCustomTemplate 自定义模板覆盖默认,并验证坏模板报错。
func TestLLMCompressorCustomTemplate(t *testing.T) {
	var got string
	c := LLMCompressor{
		Summarize: func(_ context.Context, prompt string) (string, error) {
			got = prompt
			return "s", nil
		},
		Template: "ROLES:{{range .}}{{.Role}};{{end}}",
	}
	msgs := []agent.Message{
		{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: "a"}}},
	}
	if _, err := c.Compress(context.Background(), msgs); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if got != "ROLES:user;" {
		t.Fatalf("got = %q", got)
	}
	c.Summarize = nil
	c.Template = "{{range .}}{{.Role}" // 坏模板
	if _, err := c.Compress(context.Background(), msgs); err == nil {
		t.Fatal("bad template: want error")
	}
}
