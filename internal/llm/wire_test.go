package llm

import (
	"encoding/json"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

// TestDomainToWireBlockSanitizesCorruptInput 验证 wire 层兜底:
// 历史里出现非法 RawMessage(旧会话文件/钩子注入)时,请求编码
// 必须退化为 {} 而不是炸出 "unexpected end of JSON input"。
func TestDomainToWireBlockSanitizesCorruptInput(t *testing.T) {
	corrupt := agent.NewToolUse("tu_1", "shell", json.RawMessage(`{"command":`))
	w := domainToWireBlock(corrupt)
	if string(w.Input) != "{}" {
		t.Fatalf("corrupt input must fall back to {}, got %s", w.Input)
	}

	// 合法入参原样透传。
	ok := agent.NewToolUse("tu_1", "shell", json.RawMessage(`{"command":"ls"}`))
	if got := domainToWireBlock(ok).Input; string(got) != `{"command":"ls"}` {
		t.Fatalf("valid input altered: %s", got)
	}

	// 空入参 → {}。
	empty := agent.NewToolUse("tu_1", "shell", nil)
	if got := domainToWireBlock(empty).Input; string(got) != "{}" {
		t.Fatalf("empty input = %s, want {}", got)
	}
}

// TestEncodeRequestNeverFailsOnCorruptHistory 验证完整请求编码路径:
// 消息历史含非法入参时,json.Marshal 不再失败(回归原报错)。
func TestEncodeRequestNeverFailsOnCorruptHistory(t *testing.T) {
	msgs := []agent.Message{
		{Role: agent.RoleAssistant, Blocks: []agent.Block{
			agent.NewToolUse("tu_1", "shell", json.RawMessage(`{"command":`)), // 截断入参
		}},
	}
	c := New("k", "http://unused", "m", 100)
	body, err := json.Marshal(wireRequest{
		Model: c.Model, MaxTokens: c.MaxTokens,
		Messages: domainToWireMessages(msgs),
	})
	if err != nil {
		t.Fatalf("encode request with corrupt history: %v", err)
	}
	if !json.Valid(body) {
		t.Fatalf("encoded request is not valid JSON: %s", body)
	}
}
