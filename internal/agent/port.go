package agent

import (
	"context"
	"errors"
)

// ErrSessionNotFound 表示指定会话不存在。
// Store 实现应包装(%w)返回,调用方以 errors.Is 判定。
var ErrSessionNotFound = errors.New("agent: session not found")

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

// TextDelta 是流式输出的文本增量。
type TextDelta struct {
	Text string
}

// StreamingLLM 是支持流式增量的 LLM 适配器的可选扩展接口:
// delta 只承载 text 增量(thinking/tool_use 静默组装),最终 TurnResult 与
// 非流式完全一致。Agent 在存在增量消费者且适配器支持时优先走流式。
type StreamingLLM interface {
	TurnStream(ctx context.Context, r TurnRequest, emit func(TextDelta)) (TurnResult, error)
}

// ErrMemoryNotFound 表示指定记忆键不存在。
// Store 实现应包装(%w)返回,调用方以 errors.Is 判定。
var ErrMemoryNotFound = errors.New("agent: memory not found")

// Memory 是长期记忆的 port:按键存取文本记忆。
// 值为纯文本(语义检索交给实现方);实现方负责键校验与存储格式,
// "不存在"语义统一包装 ErrMemoryNotFound 返回。
type Memory interface {
	// Put 写入/覆盖一条记忆(key 非空,value 非空)。
	Put(ctx context.Context, key, value string) error
	// Get 读取一条记忆;key 不存在时返回包装的 ErrMemoryNotFound。
	Get(ctx context.Context, key string) (string, error)
	// Search 按子串(大小写不敏感)在键与值中匹配,返回命中 key 列表(按 key 排序)。
	Search(ctx context.Context, query string) ([]string, error)
	// Keys 返回全部记忆键(按 key 排序)。
	Keys(ctx context.Context) ([]string, error)
	// Delete 删除一条记忆;不存在时返回包装的 ErrMemoryNotFound。
	Delete(ctx context.Context, key string) error
}

// SessionStore 是会话持久化的 port:按名字存取完整对话历史。
// 实现方负责名字校验与存储格式;"不存在"语义统一包装 ErrSessionNotFound 返回。
type SessionStore interface {
	Save(ctx context.Context, name string, msgs []Message) error
	Load(ctx context.Context, name string) ([]Message, error)
	Names(ctx context.Context) ([]string, error)
	Delete(ctx context.Context, name string) error
}
