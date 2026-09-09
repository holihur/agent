package tools

import (
	"context"
	"encoding/json"
	"sync/atomic"
)

// ExitFlag 记录模型是否已请求退出会话(exit 工具置位,装配层在当前轮结束后读取)。
type ExitFlag struct {
	v atomic.Bool
}

func (f *ExitFlag) Request()        { f.v.Store(true) }
func (f *ExitFlag) Requested() bool { return f.v.Load() }

// RegisterExit 注册内置 exit 工具:LLM 判断任务完成或无法继续时可主动结束会话。
// 调用后置位 flag 并向模型返回确认文本;实际退出由 REPL 循环在当前轮结束后执行,
// 保证会话保存(AfterRun)等收尾钩子先行完成。
func RegisterExit(p *LocalProvider, flag *ExitFlag) error {
	return p.Register(ToolDef{
		Name:        "exit",
		Description: "End the conversation gracefully. Call this when the task is fully complete or cannot proceed, after giving the user a final summary.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"description": "Short reason for ending the conversation (optional).",
				},
			},
		},
	}, func(_ context.Context, input json.RawMessage) (string, error) {
		flag.Request()
		var in struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(input, &in)
		if in.Reason != "" {
			return "conversation ended by exit tool: " + in.Reason, nil
		}
		return "conversation ended by exit tool", nil
	})
}
