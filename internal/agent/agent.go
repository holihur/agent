package agent

import (
	"context"
	"errors"
	"fmt"

	"agent/internal/tools"
)

// maxTurns 是防无限循环的保险丝:模型可能反复发起工具调用。
const maxTurns = 10

// ErrTooManyTurns 表示达到循环上限仍未得到最终回答。
var ErrTooManyTurns = errors.New("agent: exceeded max turns without final answer")

// stopToolUse 是 Anthropic stop_reason 中"需要执行工具"的值(协议要点 #5)。
const stopToolUse = "tool_use"

// Agent 是核心循环:调模型 → 判 tool_use → 执行 → 回填 → 再调。
type Agent struct {
	LLM      LLM             // 基础设施 port(由 cmd 注入适配器)
	Registry *tools.Registry // 工具来源(编排层依赖 tools 层)
	System   string          // system prompt(协议要点 #1:顶层字段,不进 messages)

	Messages []Message // 全部领域状态,单调追加,永不改写历史
}

// Run 执行一轮完整"思考-行动-观察"循环,返回最终文本回答。
//
// 错误分诊(设计 v2 第五节):
//   - 工具执行失败 → 不是 error,转为 is_error 的 tool_result 回填(模型消化);
//   - LLM 调用失败 / 列工具失败 / 超循环上限 → 致命 error 冒泡。
func (a *Agent) Run(ctx context.Context, user string) (string, error) {
	a.Messages = append(a.Messages, Message{Role: RoleUser, Blocks: []Block{NewText(user)}})
	for turn := 0; turn < maxTurns; turn++ {
		req, err := a.turnRequest(ctx)
		if err != nil {
			return "", fmt.Errorf("agent: list tools: %w", err)
		}
		res, err := a.LLM.Turn(ctx, req)
		if err != nil {
			return "", fmt.Errorf("agent: llm turn: %w", err)
		}
		a.Messages = append(a.Messages, res.Assistant)
		if res.StopReason != stopToolUse {
			return res.Assistant.TextContent(), nil
		}
		toolMsg, err := a.execTools(ctx, res.Assistant)
		if err != nil {
			return "", err
		}
		a.Messages = append(a.Messages, toolMsg)
	}
	return "", ErrTooManyTurns
}

// turnRequest 把当前状态投影为一次 Turn 的输入。
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
	return TurnRequest{System: a.System, Tools: specs, Messages: a.Messages}, nil
}

// execTools 顺序执行 assistant 消息里的全部 tool_use 块。
// 协议要点 #3:一轮的多个 tool_use → 合并为一条 user 消息里的多个 tool_result 块。
func (a *Agent) execTools(ctx context.Context, assistant Message) (Message, error) {
	var results []Block
	for _, b := range assistant.Blocks {
		if b.Type != BlockToolUse {
			continue
		}
		res, err := a.Registry.Call(ctx, b.Name, b.Input)
		if err != nil {
			// 工具失败不打断循环:错误文本回填给模型,让它自行决策。
			res = tools.ToolResult{Text: "error: " + err.Error(), IsError: true}
		}
		results = append(results, NewToolResult(b.ID, res.Text, res.IsError))
	}
	if len(results) == 0 {
		return Message{}, errors.New("agent: stop_reason=tool_use but assistant issued no tool_use blocks")
	}
	return Message{Role: RoleUser, Blocks: results}, nil
}
