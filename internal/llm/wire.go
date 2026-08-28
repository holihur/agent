// Package llm 是 Anthropic Messages API 适配层:唯一接触该协议 wire 格式的包。
//
// 职责:请求组装、认证头、HTTP 调用、错误体解析、领域↔wire 双向映射。
// 禁止:循环逻辑、工具执行(那是 internal/agent 的职责)。
package llm

import (
	"encoding/json"
	"fmt"

	"agent/internal/agent"
)

// ---- Anthropic Messages API wire 类型 ----
// 协议要点:
//   #1 system 是顶层字段,不进 messages
//   #2 max_tokens 必填
//   #3 工具结果以 user 角色 + tool_result 块回填(无独立 role:"tool")
//   #4 tool 入参是 JSON 对象(非字符串),且必须是对象(空参发 {})

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"` // tool_use;空时映射为 {}
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"` // tool_result(字符串形式)
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"` // thinking
	Signature string          `json:"signature,omitempty"`
}

type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// wireAPIError 是错误响应体 {"type":"error","error":{...}} 中的 error 部分。
type wireAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// APIError 是解析后的 Anthropic API 错误(致命,冒泡终止)。
type APIError struct {
	Type    string
	Message string
	Status  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic api error (%s, http %d): %s", e.Type, e.Status, e.Message)
}

// ---- 领域 → wire ----

func domainToWireMessages(msgs []agent.Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]wireBlock, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			blocks = append(blocks, domainToWireBlock(b))
		}
		out = append(out, wireMessage{Role: string(m.Role), Content: blocks})
	}
	return out
}

func domainToWireBlock(b agent.Block) wireBlock {
	w := wireBlock{
		Type:      b.Type,
		Text:      b.Text,
		ID:        b.ID,
		Name:      b.Name,
		ToolUseID: b.ToolUseID,
		Content:   b.Content,
		IsError:   b.IsError,
		Thinking:  b.Text, // thinking 块复用 Text 承载思考内容
		Signature: b.Signature,
	}
	if b.Type == agent.BlockToolUse {
		if len(b.Input) == 0 {
			w.Input = json.RawMessage("{}") // 协议要点 #4:入参必须是对象
		} else {
			w.Input = b.Input
		}
	}
	if b.Type == agent.BlockText {
		w.Thinking = "" // text 块不能带 thinking 字段
	}
	return w
}

func specsToWireTools(specs []agent.ToolSpec) []wireTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, wireTool{Name: s.Name, Description: s.Description, InputSchema: s.InputSchema})
	}
	return out
}

// ---- wire → 领域(响应) ----

func wireToDomainBlocks(ws []wireBlock) ([]agent.Block, error) {
	out := make([]agent.Block, 0, len(ws))
	for _, w := range ws {
		switch w.Type {
		case "text":
			out = append(out, agent.NewText(w.Text))
		case "tool_use":
			out = append(out, agent.NewToolUse(w.ID, w.Name, w.Input))
		case "thinking":
			// 思考块透传:回填历史时必须原样往返(含签名)。
			out = append(out, agent.Block{Type: agent.BlockThinking, Text: w.Thinking, Signature: w.Signature})
		default:
			// 其余类型(如 redacted_thinking)fail loud,给出明确类型名而非静默丢弃。
			return nil, fmt.Errorf("llm: unexpected content block type %q", w.Type)
		}
	}
	return out, nil
}
