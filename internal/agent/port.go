package agent

import "context"

// ToolSpec 是传给 LLM port 的工具投影:name/description/schema 三要素。
// 可执行函数(Provider 的实现细节)永远不过 port 边界 —— 分层纪律。
type ToolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// TurnRequest 是一轮"思考"的全部输入。
// model / max_tokens / api key 是适配器构造参数,不随每轮传递。
type TurnRequest struct {
	System   string
	Tools    []ToolSpec
	Messages []Message
}

// TurnResult 返回 assistant 消息与停止原因("tool_use" / "end_turn" / ...)。
type TurnResult struct {
	Assistant  Message
	StopReason string
}

// LLM 是编排层唯一能看到的基础设施抽象(port)。
// internal/llm 实现它;将来第二协议 = 新增一个适配器,本包零改动。
type LLM interface {
	Turn(ctx context.Context, r TurnRequest) (TurnResult, error)
}
