// Package agent 实现最小 Agent 的编排层:领域类型、LLM port 与核心循环。
//
// 分层纪律:本包不出现任何 HTTP/JSON wire 字段名;
// 序列化由 internal/llm(wire 层)负责,本包类型不带 json tag。
package agent

import (
	"encoding/json"
	"strings"
)

// Role 是消息角色;Anthropic Messages API 只允许 user / assistant。
// 工具结果以 user 角色的 tool_result 块回填(协议要点 #3)。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Block 类型常量(领域层支持的块;其余类型由 wire 层拒绝)。
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
	BlockThinking   = "thinking"
)

// Block 是消息内容的最小单元(扁平结构;块类型增多时再拆分类型)。
type Block struct {
	Type      string          // 块类型:BlockText / BlockToolUse / BlockToolResult / BlockThinking
	Text      string          // text(或 thinking 的思考内容)
	ID        string          // tool_use:工具调用 ID
	Name      string          // tool_use:工具名
	Input     json.RawMessage // tool_use:模型给出的入参(JSON 对象,协议要点 #4)
	ToolUseID string          // tool_result:对应的 tool_use ID
	Content   string          // tool_result:回填文本
	IsError   bool            // tool_result:工具失败标记(让模型区分成败)
	Signature string          // thinking:签名(回填历史时必须原样往返)
}

func NewText(s string) Block { return Block{Type: BlockText, Text: s} }

func NewToolUse(id, name string, input json.RawMessage) Block {
	return Block{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

func NewToolResult(toolUseID, content string, isError bool) Block {
	return Block{Type: BlockToolResult, ToolUseID: toolUseID, Content: content, IsError: isError}
}

// Message 是一条对话消息:role + 有序块序列。Agent 的全部领域状态即 []Message。
type Message struct {
	Role   Role
	Blocks []Block
}

// TextContent 拼接消息中所有 text 块(以换行连接),作为最终回答。
func (m Message) TextContent() string {
	parts := make([]string, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		if b.Type == BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
