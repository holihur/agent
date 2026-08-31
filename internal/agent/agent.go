package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/holihur/agent/internal/tools"
)

// maxTurns 是防无限循环的默认保险丝:模型可能反复发起工具调用;
// 可经 Agent.MaxTurns 按次覆盖。
const maxTurns = 60

// ErrTooManyTurns 表示达到循环上限仍未得到最终回答。
var ErrTooManyTurns = errors.New("agent: exceeded max turns without final answer")

// stopToolUse 是 Anthropic stop_reason 中"需要执行工具"的值(协议要点 #5)。
const stopToolUse = "tool_use"

// Agent 是核心循环:调模型 → 判 tool_use → 执行 → 回填 → 再调。
// 功能扩展一律走 Hooks(见 hooks.go 的包契约),核心循环不再改动。
type Agent struct {
	LLM      LLM             // 基础设施 port(由 cmd 注入适配器)
	Registry *tools.Registry // 工具来源(编排层依赖 tools 层)
	System   string          // system prompt(协议要点 #1:顶层字段,不进 messages)
	Hooks    *Hooks          // 生命周期钩子;nil = 无钩子(全部分发方法 nil 安全)
	// MaxTurns 是单次 Run 的循环上限(防无限循环保险丝);零值/负值回落到默认 maxTurns。
	MaxTurns int
	// OnTextDelta 非 nil 且 LLM 支持 StreamingLLM 时,文本增量经此发出;
	// 最终结果不受影响(流式只是传输形态)。
	OnTextDelta func(TextDelta)

	Messages []Message // 全部领域状态,单调追加,永不改写历史
}

// Run 执行一轮完整"思考-行动-观察"循环,返回最终文本回答。
// 输入拦截器(如 "!" shell 逃逸)命中的输入不进对话:既不调模型也不写历史。
//
// 错误分诊(设计 v2 第五节):
//   - 工具执行失败/被钩子拒绝 → 不是 error,转为 is_error 的 tool_result 回填(模型消化);
//   - LLM 调用失败 / 列工具失败 / 超循环上限 → 致命 error 冒泡。
func (a *Agent) Run(ctx context.Context, user string) (string, error) {
	a.Hooks.emitRunStart(UserInput{Text: user})
	if out, handled := a.Hooks.interceptInput(user); handled {
		a.Hooks.emitRunEnd(RunOutcome{Answer: out, Turns: 0})
		return out, nil
	}
	user = a.Hooks.chainUserInput(user)
	a.Messages = append(a.Messages, Message{Role: RoleUser, Blocks: []Block{NewText(user)}})
	limit := a.maxTurnsLimit()
	turns := 0
	for turn := 0; turn < limit; turn++ {
		turns = turn + 1
		req, err := a.turnRequest(ctx)
		if err != nil {
			err = fmt.Errorf("agent: list tools: %w", err)
			a.Hooks.emitRunEnd(RunOutcome{Err: err, Turns: turns})
			return "", err
		}
		req = a.Hooks.chainTurnRequest(req)
		a.Hooks.emitBeforeLLM(TurnStat{Turn: turn, Messages: len(req.Messages), Tools: len(req.Tools)})
		res, err := a.turn(ctx, req)
		if err != nil {
			err = fmt.Errorf("agent: llm turn: %w", err)
			a.Hooks.emitRunEnd(RunOutcome{Err: err, Turns: turns})
			return "", err
		}
		a.Hooks.emitAfterLLM(TurnStat{
			Turn: turn, Messages: len(req.Messages), Tools: len(req.Tools),
			StopReason: res.StopReason, Blocks: len(res.Assistant.Blocks),
		})
		res.Assistant = a.Hooks.chainAssistant(res.Assistant)
		a.Messages = append(a.Messages, res.Assistant)
		if res.StopReason != stopToolUse {
			answer := a.Hooks.chainAnswer(res.Assistant.TextContent())
			a.Hooks.emitRunEnd(RunOutcome{Answer: answer, Turns: turns})
			return answer, nil
		}
		toolMsg, err := a.execTools(ctx, res.Assistant, turn)
		if err != nil {
			a.Hooks.emitRunEnd(RunOutcome{Err: err, Turns: turns})
			return "", err
		}
		a.Messages = append(a.Messages, toolMsg)
	}
	a.Hooks.emitRunEnd(RunOutcome{Err: ErrTooManyTurns, Turns: limit})
	return "", ErrTooManyTurns
}

// maxTurnsLimit 返回本次 Run 的生效循环上限:显式配置优先,否则默认保险丝。
func (a *Agent) maxTurnsLimit() int {
	if a.MaxTurns > 0 {
		return a.MaxTurns
	}
	return maxTurns
}

// turn 按能力选择调用形态:有增量消费者且适配器支持流式时走 TurnStream,否则 Turn。
func (a *Agent) turn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if a.OnTextDelta != nil {
		if s, ok := a.LLM.(StreamingLLM); ok {
			return s.TurnStream(ctx, req, a.OnTextDelta)
		}
	}
	return a.LLM.Turn(ctx, req)
}

// turnRequest 把当前状态投影为一次 Turn 的输入。
// Messages 做浅拷贝,防止钩子对请求的 reslice 波及持久状态。
// 工具列表每次投影时聚合;MCP 源的缓存/分页由各自 Provider 自理。
func (a *Agent) turnRequest(ctx context.Context) (TurnRequest, error) {
	defs, err := a.Registry.Tools(ctx)
	if err != nil {
		return TurnRequest{}, err
	}
	specs := make([]ToolSpec, 0, len(defs))
	for _, d := range defs {
		specs = append(specs, ToolSpec{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema})
	}
	return TurnRequest{System: a.System, Tools: specs, Messages: cloneMessages(a.Messages)}, nil
}

// execTools 顺序执行 assistant 消息里的全部 tool_use 块。
// 协议要点 #3:一轮的多个 tool_use → 合并为一条 user 消息里的多个 tool_result 块。
// 执行链:MutateToolInput → BeforeTool 裁决(可拒绝) → 执行 → MutateToolResult → 回填。
func (a *Agent) execTools(ctx context.Context, assistant Message, turn int) (Message, error) {
	var results []Block
	for _, b := range assistant.Blocks {
		if b.Type != BlockToolUse {
			continue
		}
		call := a.Hooks.chainToolInput(ToolCall{Turn: turn, Name: b.Name, Input: b.Input})
		start := time.Now()
		if dec := a.Hooks.gateTool(call); dec.Deny {
			results = append(results, NewToolResult(b.ID, "denied: "+dec.Reason, true))
			a.Hooks.emitAfterTool(ToolOutcome{Turn: turn, Name: b.Name, Text: dec.Reason, Denied: true})
			continue
		}
		res, err := a.Registry.Call(ctx, call.Name, call.Input)
		if err != nil {
			// 工具失败不打断循环:错误文本回填给模型,让它自行决策。
			res = tools.ToolResult{Text: "error: " + err.Error(), IsError: true}
		}
		res = a.Hooks.chainToolResult(res)
		a.Hooks.emitAfterTool(ToolOutcome{
			Turn: turn, Name: b.Name, Text: res.Text, IsError: res.IsError,
			Err: err, Duration: time.Since(start),
		})
		results = append(results, NewToolResult(b.ID, res.Text, res.IsError))
	}
	if len(results) == 0 {
		return Message{}, errors.New("agent: stop_reason=tool_use but assistant issued no tool_use blocks")
	}
	return Message{Role: RoleUser, Blocks: results}, nil
}
