package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/holihur/agent/internal/agent"
)

// ResponsesClient 实现 OpenAI Responses API(新接口)适配器。
// 端点:POST {BaseURL}/v1/responses;认证:Bearer。
// 领域模型仍是 agent.Message / agent.Block;此处负责领域 ↔ Responses wire 映射。
type ResponsesClient struct {
	APIKey          string
	BaseURL         string
	Model           string
	MaxTokens       int
	HTTP            *http.Client
	Temperature     *float64 // nil = 不发送(端点默认)
	ReasoningEffort string   // 空 = 不发送;透传为 reasoning.effort
}

func NewResponses(apiKey, baseURL, model string, maxTokens int) *ResponsesClient {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &ResponsesClient{APIKey: apiKey, BaseURL: baseURL, Model: model, MaxTokens: maxTokens}
}

// ---- Responses API wire 类型 ----
// 要点:
//   #1 system 转顶层 instructions 字段
//   #2 工具结果以 type:"function_call_output" 项回填(call_id 关联)
//   #3 tool_use 转为 type:"function_call" 项,arguments 为 JSON 字符串
//   #4 输出 token 上限字段名是 max_output_tokens(非 max_tokens)

type responsesRequest struct {
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions,omitempty"`
	Input           []responsesItem     `json:"input"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	Stream          bool                `json:"stream,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type responsesTool struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// responsesItem 是 input 数组的一项:普通消息带 role+content;
// function_call / function_call_output 是顶层项(非消息内嵌块)。
type responsesItem struct {
	Type      string `json:"type,omitempty"`      // "function_call" | "function_call_output"
	Role      string `json:"role,omitempty"`      // user / assistant
	Content   any    `json:"content,omitempty"`   // 消息文本(string)
	CallID    string `json:"call_id,omitempty"`   // function_call / function_call_output
	Name      string `json:"name,omitempty"`      // function_call
	Arguments string `json:"arguments,omitempty"` // function_call
	Output    string `json:"output,omitempty"`    // function_call_output
}

type responsesResponse struct {
	ID     string                `json:"id"`
	Status string                `json:"status"`
	Output []responsesOutputItem `json:"output"`
	Usage  *responsesUsage       `json:"usage"`
	Error  *openaiError          `json:"error"`
}

type responsesOutputItem struct {
	Type      string                 `json:"type"` // message | function_call | reasoning | ...
	Role      string                 `json:"role,omitempty"`
	Content   []responsesContentPart `json:"content,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// ---- 领域 → wire ----

func domainToResponsesItems(system string, msgs []agent.Message) (string, []responsesItem) {
	items := make([]responsesItem, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case agent.RoleUser:
			for _, b := range m.Blocks {
				switch b.Type {
				case agent.BlockToolResult:
					items = append(items, responsesItem{Type: "function_call_output", CallID: b.ToolUseID, Output: b.Content})
				case agent.BlockText:
					items = append(items, responsesItem{Role: "user", Content: b.Text})
				}
			}
		case agent.RoleAssistant:
			for _, b := range m.Blocks {
				switch b.Type {
				case agent.BlockText:
					items = append(items, responsesItem{Role: "assistant", Content: b.Text})
				case agent.BlockToolUse:
					items = append(items, responsesItem{
						Type:      "function_call",
						CallID:    b.ID,
						Name:      b.Name,
						Arguments: chatArgumentsString(string(b.Input)),
					})
				}
			}
		}
	}
	return system, items
}

func specsToResponsesTools(specs []agent.ToolSpec) []responsesTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]responsesTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, responsesTool{Type: "function", Name: s.Name, Description: s.Description, Parameters: s.InputSchema})
	}
	return out
}

func responsesReasoningPtr(effort string) *responsesReasoning {
	if effort == "" {
		return nil
	}
	return &responsesReasoning{Effort: effort}
}

// ---- wire → 领域(响应) ----

func responsesToResult(rr responsesResponse) (agent.TurnResult, error) {
	blocks := make([]agent.Block, 0, len(rr.Output))
	toolUse := false
	for _, item := range rr.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text", "text":
					if part.Text != "" {
						blocks = append(blocks, agent.NewText(part.Text))
					}
				}
			}
		case "function_call":
			toolUse = true
			blocks = append(blocks, agent.NewToolUse(item.CallID, item.Name, json.RawMessage(chatArgumentsString(item.Arguments))))
		}
		// reasoning 等其余输出项无领域对应,静默跳过。
	}
	stopReason := "end_turn"
	if toolUse {
		stopReason = "tool_use" // Responses 无 finish_reason,以输出中是否有 function_call 判定
	}
	usage := agent.TokenUsage{}
	if rr.Usage != nil {
		usage.Input = rr.Usage.InputTokens
		usage.Output = rr.Usage.OutputTokens
		usage.CacheRead = rr.Usage.InputTokensDetails.CachedTokens
	}
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: blocks},
		StopReason: stopReason,
		Usage:      usage,
	}, nil
}

// Turn 发起一次 /v1/responses 调用。
func (c *ResponsesClient) Turn(ctx context.Context, r agent.TurnRequest) (agent.TurnResult, error) {
	if c.BaseURL == "" {
		return agent.TurnResult{}, errors.New("llm: base URL is required")
	}
	system, items := domainToResponsesItems(r.System, r.Messages)
	body, err := json.Marshal(responsesRequest{
		Model:           c.Model,
		Instructions:    system,
		Input:           items,
		Tools:           specsToResponsesTools(r.Tools),
		MaxOutputTokens: c.MaxTokens,
		Temperature:     c.Temperature,
		Reasoning:       responsesReasoningPtr(c.ReasoningEffort),
	})
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := openAIPost(ctx, c.HTTP, c.APIKey, c.BaseURL+"/v1/responses", body)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return agent.TurnResult{}, openAIErrorFromResponse(resp)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: read response: %w", err)
	}
	var rr responsesResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if rr.Error != nil {
		return agent.TurnResult{}, &APIError{Type: rr.Error.Type, Message: rr.Error.Message, Status: resp.StatusCode}
	}
	return responsesToResult(rr)
}

// ---- 流式 ----

// responsesStreamEvent 是 Responses SSE data 行的信封;type 即事件名。
type responsesStreamEvent struct {
	Type        string               `json:"type"`
	OutputIndex int                  `json:"output_index"`
	Delta       string               `json:"delta"`
	Text        string               `json:"text"`
	Arguments   string               `json:"arguments"`
	Item        *responsesOutputItem `json:"item"`
	Response    *responsesResponse   `json:"response"`
}

// responsesStreamAssembler 按 output_index 累积 text 与 function_call(arguments 分片)。
type responsesStreamAssembler struct {
	emit func(agent.TextDelta)

	textByIndex map[int]*strings.Builder
	callByIndex map[int]*responsesCallBuilder
	maxIndex    int
	sawToolUse  bool
	usage       agent.TokenUsage
}

type responsesCallBuilder struct {
	callID string
	name   string
	args   strings.Builder
}

func (s *responsesStreamAssembler) consume(r io.Reader) error {
	return scanSSE(r, s.handle)
}

func (s *responsesStreamAssembler) handle(payload string) error {
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var ev responsesStreamEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return fmt.Errorf("llm: decode stream event: %w", err)
	}
	if ev.OutputIndex > s.maxIndex {
		s.maxIndex = ev.OutputIndex
	}
	switch ev.Type {
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			s.sawToolUse = true
			b := s.callBuilder(ev.OutputIndex)
			b.callID = ev.Item.CallID
			b.name = ev.Item.Name
		}
	case "response.output_text.delta":
		s.textBuilder(ev.OutputIndex).WriteString(ev.Delta)
		if s.emit != nil {
			s.emit(agent.TextDelta{Text: ev.Delta})
		}
	case "response.output_text.done":
		if ev.Text != "" {
			b := s.textBuilder(ev.OutputIndex)
			b.Reset()
			b.WriteString(ev.Text)
		}
	case "response.function_call_arguments.delta":
		s.callBuilder(ev.OutputIndex).args.WriteString(ev.Delta)
	case "response.function_call_arguments.done":
		if ev.Arguments != "" {
			b := s.callBuilder(ev.OutputIndex)
			b.args.Reset()
			b.args.WriteString(ev.Arguments)
		}
	case "response.output_item.done":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			s.sawToolUse = true
			b := s.callBuilder(ev.OutputIndex)
			if ev.Item.CallID != "" {
				b.callID = ev.Item.CallID
			}
			if ev.Item.Name != "" {
				b.name = ev.Item.Name
			}
			if ev.Item.Arguments != "" {
				b.args.Reset()
				b.args.WriteString(ev.Item.Arguments)
			}
		}
	case "response.completed":
		if ev.Response != nil && ev.Response.Usage != nil {
			s.usage.Input = ev.Response.Usage.InputTokens
			s.usage.Output = ev.Response.Usage.OutputTokens
			s.usage.CacheRead = ev.Response.Usage.InputTokensDetails.CachedTokens
		}
	case "response.failed":
		if ev.Response != nil && ev.Response.Error != nil {
			return &APIError{Type: ev.Response.Error.Type, Message: ev.Response.Error.Message}
		}
		return fmt.Errorf("llm: responses stream failed")
	}
	return nil
}

func (s *responsesStreamAssembler) textBuilder(idx int) *strings.Builder {
	if s.textByIndex == nil {
		s.textByIndex = map[int]*strings.Builder{}
	}
	b, ok := s.textByIndex[idx]
	if !ok {
		b = &strings.Builder{}
		s.textByIndex[idx] = b
	}
	return b
}

func (s *responsesStreamAssembler) callBuilder(idx int) *responsesCallBuilder {
	if s.callByIndex == nil {
		s.callByIndex = map[int]*responsesCallBuilder{}
	}
	b, ok := s.callByIndex[idx]
	if !ok {
		b = &responsesCallBuilder{}
		s.callByIndex[idx] = b
	}
	return b
}

func (s *responsesStreamAssembler) result() agent.TurnResult {
	blocks := make([]agent.Block, 0, s.maxIndex+1)
	for i := 0; i <= s.maxIndex; i++ {
		if b := s.callAt(i); b != nil {
			blocks = append(blocks, agent.NewToolUse(b.callID, b.name, json.RawMessage(chatArgumentsString(b.args.String()))))
			continue
		}
		if tb := s.textAt(i); tb != nil && tb.Len() > 0 {
			blocks = append(blocks, agent.NewText(tb.String()))
		}
	}
	stopReason := "end_turn"
	if s.sawToolUse {
		stopReason = "tool_use"
	}
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: blocks},
		StopReason: stopReason,
		Usage:      s.usage,
	}
}

func (s *responsesStreamAssembler) callAt(idx int) *responsesCallBuilder {
	if s.callByIndex == nil {
		return nil
	}
	return s.callByIndex[idx]
}

func (s *responsesStreamAssembler) textAt(idx int) *strings.Builder {
	if s.textByIndex == nil {
		return nil
	}
	return s.textByIndex[idx]
}

// TurnStream 以 SSE 流式调用 Responses API(stream:true)。
func (c *ResponsesClient) TurnStream(ctx context.Context, r agent.TurnRequest, emit func(agent.TextDelta)) (agent.TurnResult, error) {
	system, items := domainToResponsesItems(r.System, r.Messages)
	body, err := json.Marshal(responsesRequest{
		Model:           c.Model,
		Instructions:    system,
		Input:           items,
		Tools:           specsToResponsesTools(r.Tools),
		MaxOutputTokens: c.MaxTokens,
		Temperature:     c.Temperature,
		Reasoning:       responsesReasoningPtr(c.ReasoningEffort),
		Stream:          true,
	})
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := openAIPost(ctx, c.HTTP, c.APIKey, c.BaseURL+"/v1/responses", body)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return agent.TurnResult{}, openAIErrorFromResponse(resp)
	}
	s := &responsesStreamAssembler{emit: emit}
	if err := s.consume(resp.Body); err != nil {
		return agent.TurnResult{}, err
	}
	return s.result(), nil
}
