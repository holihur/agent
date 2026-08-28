// Package tools 定义工具的领域模型与两个 port:
//
//	Provider  —— 工具从哪来(本地函数与 MCP 服务器平权)
//	Responder —— 工具执行中的追问谁来答(MRTR 的领域投影)
//
// 本包不知道 MCP、HTTP 或任何消息格式的存在。
package tools

import (
	"context"
	"encoding/json"
)

// ToolDef 是暴露给模型的工具定义(name/description/inputSchema 三要素)。
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolResult 是工具执行结果:回填文本 + 失败标记。
// 工具级失败不是 Go error —— 由 IsError 传给模型自行消化。
type ToolResult struct {
	Text    string
	IsError bool
}

// Provider 是一个工具来源:local(进程内函数)或一个 MCP 服务器。
type Provider interface {
	// Namespace 返回来源命名空间;"local" 不加前缀,其余以 "<ns>__<tool>" 暴露。
	Namespace() string
	// ListTools 列出本源当前可用工具(缓存/分页由实现者自理)。
	ListTools(ctx context.Context) ([]ToolDef, error)
	// CallTool 执行一个本源工具(name 为未加前缀的原始名)。
	CallTool(ctx context.Context, name string, input json.RawMessage) (ToolResult, error)
}

// InputField 是追问中需要用户填的一项。
type InputField struct {
	Name        string
	Description string
	Required    bool
}

// InputPrompt 是一次追问(MCP elicitation 形状的领域投影)。
type InputPrompt struct {
	Key     string       // 追问标识(MRTR inputRequests 的 key)
	Message string       // 问什么
	Fields  []InputField // 需要什么字段
}

// InputRequest 是一次工具执行引发的全部追问。
type InputRequest struct {
	Tool    string
	Prompts []InputPrompt
}

// InputResponse 是用户对某个 Key 追问的作答(字段名 → 值)。
type InputResponse struct {
	Key     string
	Content map[string]any
}

// Responder 是"谁来回答工具追问"的 port。
// CLI/TUI/API 各自实现;返回 error 视为拒绝作答。
type Responder interface {
	Respond(ctx context.Context, req InputRequest) ([]InputResponse, error)
}
