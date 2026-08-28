package agent

import (
	"encoding/json"
	"slices"
	"time"

	"agent/internal/tools"
)

// Hooks 是 Agent 的扩展缝隙:功能扩展以注册钩子的形式提供,核心循环不再改动。
//
// 三类钩子:
//   - Mutate*(管道):按注册顺序串联,前一个的输出是后一个的输入,可改写内容;
//   - BeforeTool(门):返回 Decision 可拒绝执行;
//   - 其余(观测):只读事件。
//
// 状态一致性契约:
//   - MutateTurnRequest / MutateToolInput 只影响本次出站调用,
//     Agent.Messages 始终是持久真源;
//   - MutateUserInput / MutateAssistant / MutateToolResult 发生在入史前,
//     其结果即为持久状态;MutateAnswer 只改返回值;
//   - 载荷中的切片不得原地修改(以返回新切片表达变更)。
//
// 所有方法 nil 安全(Hooks 允许为 nil);Mutate 管道与观测按注册顺序执行,
// BeforeTool 首个 Deny 短路。
type Hooks struct {
	mutateUserInput   []func(string) string
	mutateTurnRequest []func(TurnRequest) TurnRequest
	mutateToolInput   []func(ToolCall) ToolCall
	mutateToolResult  []func(tools.ToolResult) tools.ToolResult
	mutateAssistant   []func(Message) Message
	mutateAnswer      []func(string) string

	beforeTool []func(ToolCall) Decision

	runStart  []func(UserInput)
	beforeLLM []func(TurnStat)
	afterLLM  []func(TurnStat)
	afterTool []func(ToolOutcome)
	runEnd    []func(RunOutcome)
}

func NewHooks() *Hooks { return &Hooks{} }

// ToolCall 是一次待执行的工具调用(供 MutateToolInput 与 BeforeTool 使用)。
type ToolCall struct {
	Turn  int
	Name  string          // 暴露名(含命名空间前缀)
	Input json.RawMessage // 模型给出的入参
}

// Decision 是 BeforeTool 的裁决;零值 = 放行。
type Decision struct {
	Deny   bool
	Reason string
}

// UserInput 是一次 Run 收到的用户输入。
type UserInput struct {
	Text string
}

// TurnStat 是一轮 LLM 调用的统计;before 时 StopReason/Blocks 为零值。
type TurnStat struct {
	Turn       int // 0 起
	Messages   int
	Tools      int
	StopReason string // 仅 after_llm
	Blocks     int    // 仅 after_llm
}

// ToolOutcome 是一次工具调用的结局(执行或被拒),只读观测。
type ToolOutcome struct {
	Turn     int
	Name     string
	Text     string
	IsError  bool
	Denied   bool          // 被 before_tool 钩子拒绝,未执行
	Err      error         // 执行错误(该错误已转 is_error 回填,此处仅为观测)
	Duration time.Duration // 被拒时为 0
}

// RunOutcome 是一次 Run 的结局(Answer 与 Err 互斥)。
type RunOutcome struct {
	Answer string
	Err    error
	Turns  int
}

// ---- 注册 ----

func (h *Hooks) OnMutateUserInput(fn func(string) string) {
	if h != nil {
		h.mutateUserInput = append(h.mutateUserInput, fn)
	}
}

func (h *Hooks) OnMutateTurnRequest(fn func(TurnRequest) TurnRequest) {
	if h != nil {
		h.mutateTurnRequest = append(h.mutateTurnRequest, fn)
	}
}

func (h *Hooks) OnMutateToolInput(fn func(ToolCall) ToolCall) {
	if h != nil {
		h.mutateToolInput = append(h.mutateToolInput, fn)
	}
}

func (h *Hooks) OnMutateToolResult(fn func(tools.ToolResult) tools.ToolResult) {
	if h != nil {
		h.mutateToolResult = append(h.mutateToolResult, fn)
	}
}

func (h *Hooks) OnMutateAssistant(fn func(Message) Message) {
	if h != nil {
		h.mutateAssistant = append(h.mutateAssistant, fn)
	}
}

func (h *Hooks) OnMutateAnswer(fn func(string) string) {
	if h != nil {
		h.mutateAnswer = append(h.mutateAnswer, fn)
	}
}

func (h *Hooks) OnBeforeTool(fn func(ToolCall) Decision) {
	if h != nil {
		h.beforeTool = append(h.beforeTool, fn)
	}
}

func (h *Hooks) OnRunStart(fn func(UserInput)) {
	if h != nil {
		h.runStart = append(h.runStart, fn)
	}
}

func (h *Hooks) OnBeforeLLM(fn func(TurnStat)) {
	if h != nil {
		h.beforeLLM = append(h.beforeLLM, fn)
	}
}

func (h *Hooks) OnAfterLLM(fn func(TurnStat)) {
	if h != nil {
		h.afterLLM = append(h.afterLLM, fn)
	}
}

func (h *Hooks) OnAfterTool(fn func(ToolOutcome)) {
	if h != nil {
		h.afterTool = append(h.afterTool, fn)
	}
}

func (h *Hooks) OnRunEnd(fn func(RunOutcome)) {
	if h != nil {
		h.runEnd = append(h.runEnd, fn)
	}
}

// ---- 分发(nil 安全;包内使用) ----

func (h *Hooks) chainUserInput(v string) string {
	if h == nil {
		return v
	}
	for _, fn := range h.mutateUserInput {
		v = fn(v)
	}
	return v
}

func (h *Hooks) chainTurnRequest(r TurnRequest) TurnRequest {
	if h == nil {
		return r
	}
	for _, fn := range h.mutateTurnRequest {
		r = fn(r)
	}
	return r
}

func (h *Hooks) chainToolInput(c ToolCall) ToolCall {
	if h == nil {
		return c
	}
	for _, fn := range h.mutateToolInput {
		c = fn(c)
	}
	return c
}

func (h *Hooks) chainToolResult(r tools.ToolResult) tools.ToolResult {
	if h == nil {
		return r
	}
	for _, fn := range h.mutateToolResult {
		r = fn(r)
	}
	return r
}

func (h *Hooks) chainAssistant(m Message) Message {
	if h == nil {
		return m
	}
	for _, fn := range h.mutateAssistant {
		m = fn(m)
	}
	return m
}

func (h *Hooks) chainAnswer(v string) string {
	if h == nil {
		return v
	}
	for _, fn := range h.mutateAnswer {
		v = fn(v)
	}
	return v
}

func (h *Hooks) gateTool(c ToolCall) Decision {
	if h == nil {
		return Decision{}
	}
	for _, fn := range h.beforeTool {
		if d := fn(c); d.Deny {
			return d
		}
	}
	return Decision{}
}

func (h *Hooks) emitRunStart(e UserInput) {
	if h == nil {
		return
	}
	for _, fn := range h.runStart {
		fn(e)
	}
}

func (h *Hooks) emitBeforeLLM(e TurnStat) {
	if h == nil {
		return
	}
	for _, fn := range h.beforeLLM {
		fn(e)
	}
}

func (h *Hooks) emitAfterLLM(e TurnStat) {
	if h == nil {
		return
	}
	for _, fn := range h.afterLLM {
		fn(e)
	}
}

func (h *Hooks) emitAfterTool(e ToolOutcome) {
	if h == nil {
		return
	}
	for _, fn := range h.afterTool {
		fn(e)
	}
}

func (h *Hooks) emitRunEnd(e RunOutcome) {
	if h == nil {
		return
	}
	for _, fn := range h.runEnd {
		fn(e)
	}
}

// cloneMessages 浅拷贝消息切片,防止钩子对请求的 reslice/append 波及持久状态。
func cloneMessages(msgs []Message) []Message {
	return slices.Clone(msgs)
}
