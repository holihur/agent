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

// ChatClient 实现 OpenAI Chat Completions API(老接口)适配器。
// 端点:POST {BaseURL}/v1/chat/completions;认证:Bearer。
// 领域模型仍是 agent.Message / agent.Block;此处负责领域 ↔ Chat Completions wire 映射。
type ChatClient struct {
	APIKey          string
	BaseURL         string
	Model           string
	MaxTokens       int
	HTTP            *http.Client
	Temperature     *float64 // nil = 不发送(端点默认)
	ReasoningEffort string   // 空 = 不发送;透传给 reasoning_effort(推理模型)
}

func NewChat(apiKey, baseURL, model string, maxTokens int) *ChatClient {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &ChatClient{APIKey: apiKey, BaseURL: baseURL, Model: model, MaxTokens: maxTokens}
}

// ---- Chat Completions wire 类型 ----
// 要点:
//   #1 system 转为首条 role:"system" 消息
//   #2 工具结果以 role:"tool" + tool_call_id 回填(每块一条)
//   #3 tool_use 入参转为 arguments JSON 字符串(非对象)
//   #4 思考内容经 reasoning_content 原样回传(DeepSeek 思维模式带 tools 时强制要求)

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string           `json:"type"` // "function"
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	Tools           []chatTool    `json:"tools,omitempty"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Stream          bool          `json:"stream,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`           // 字符串或 null(纯工具调用时)
			ReasoningContent string          `json:"reasoning_content"` // DeepSeek/LongCat 等推理模型的思考内容
			ToolCalls        []chatToolCall  `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage   `json:"usage"`
	Error *openaiError `json:"error"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---- 领域 → wire ----

func domainToChatMessages(system string, msgs []agent.Message) []chatMessage {
	out := make([]chatMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case agent.RoleUser:
			// 工具结果逐块转为独立 tool 消息;混在其中的 text 块合并为一条 user 消息。
			var texts []string
			for _, b := range m.Blocks {
				switch b.Type {
				case agent.BlockToolResult:
					out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
				case agent.BlockText:
					texts = append(texts, b.Text)
				}
			}
			if len(texts) > 0 {
				out = append(out, chatMessage{Role: "user", Content: strings.Join(texts, "\n")})
			}
		case agent.RoleAssistant:
			var texts []string
			var reasonings []string
			var calls []chatToolCall
			for _, b := range m.Blocks {
				switch b.Type {
				case agent.BlockText:
					texts = append(texts, b.Text)
				case agent.BlockThinking:
					// DeepSeek 思维模式:带 tools 时上一轮的 reasoning_content 必须原样回传,
					// 否则 400。thinking 块承载该内容,这里重建为 reasoning_content。
					reasonings = append(reasonings, b.Text)
				case agent.BlockToolUse:
					calls = append(calls, chatToolCall{
						ID:       b.ID,
						Type:     "function",
						Function: chatFunction{Name: b.Name, Arguments: chatArgumentsString(string(b.Input))},
					})
				}
			}
			if len(texts) == 0 && len(calls) == 0 && len(reasonings) == 0 {
				continue // 空 assistant 消息直接跳过
			}
			out = append(out, chatMessage{
				Role:             "assistant",
				Content:          strings.Join(texts, "\n"),
				ReasoningContent: strings.Join(reasonings, ""),
				ToolCalls:        calls,
			})
		}
	}
	return out
}

func specsToChatTools(specs []agent.ToolSpec) []chatTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]chatTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, chatTool{
			Type:     "function",
			Function: chatToolFunction{Name: s.Name, Description: s.Description, Parameters: s.InputSchema},
		})
	}
	return out
}

// ---- wire → 领域(响应) ----

func chatResponseToResult(cr chatResponse) (agent.TurnResult, error) {
	if len(cr.Choices) == 0 {
		return agent.TurnResult{}, errors.New("llm: chat completion returned no choices")
	}
	choice := cr.Choices[0]
	blocks := make([]agent.Block, 0, len(choice.Message.ToolCalls)+2)
	// 思考内容映射为 thinking 块:回填历史时会被 domainToChatMessages 静默丢弃,
	// 不回传给模型;但入史/会话保存得以保留,且 content 为空时可回退显示。
	if reasoning := choice.Message.ReasoningContent; reasoning != "" {
		blocks = append(blocks, agent.Block{Type: agent.BlockThinking, Text: reasoning})
	}
	if text := rawContentText(choice.Message.Content); text != "" {
		blocks = append(blocks, agent.NewText(text))
	}
	for _, tc := range choice.Message.ToolCalls {
		blocks = append(blocks, agent.NewToolUse(tc.ID, tc.Function.Name, json.RawMessage(chatArgumentsString(tc.Function.Arguments))))
	}
	stopReason := choice.FinishReason
	if stopReason == "" {
		stopReason = "end_turn"
	} else if stopReason == "tool_calls" {
		stopReason = "tool_use" // 归一为 agent 循环识别的哨兵
	}
	usage := agent.TokenUsage{}
	if cr.Usage != nil {
		usage.Input = cr.Usage.PromptTokens
		usage.Output = cr.Usage.CompletionTokens
	}
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: blocks},
		StopReason: stopReason,
		Usage:      usage,
	}, nil
}

// rawContentText 提取 content 字段:字符串 → 文本;null/数组等其他形态 → 空串。
func rawContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Turn 发起一次 /v1/chat/completions 调用。
func (c *ChatClient) Turn(ctx context.Context, r agent.TurnRequest) (agent.TurnResult, error) {
	if c.BaseURL == "" {
		return agent.TurnResult{}, errors.New("llm: base URL is required")
	}
	body, err := json.Marshal(chatRequest{
		Model:           c.Model,
		Messages:        domainToChatMessages(r.System, r.Messages),
		Tools:           specsToChatTools(r.Tools),
		MaxTokens:       c.MaxTokens,
		Temperature:     c.Temperature,
		ReasoningEffort: c.ReasoningEffort,
	})
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := openAIPost(ctx, c.HTTP, c.APIKey, c.BaseURL+"/v1/chat/completions", body)
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
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if cr.Error != nil {
		return agent.TurnResult{}, &APIError{Type: cr.Error.Type, Message: cr.Error.Message, Status: resp.StatusCode}
	}
	return chatResponseToResult(cr)
}

// ---- 流式 ----

// chatStreamEvent 是 SSE data 行的信封(choices[0].delta 承载增量)。
type chatStreamEvent struct {
	Choices []struct {
		Delta struct {
			Role             string              `json:"role"`
			Content          string              `json:"content"`
			ReasoningContent string              `json:"reasoning_content"`
			ToolCalls        []chatToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

type chatToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatStreamAssembler 按 SSE 帧累积 text 与 tool_calls(arguments 分片到达)。
type chatStreamAssembler struct {
	emit func(agent.TextDelta)

	text       strings.Builder
	reasoning  strings.Builder
	stopReason string
	usage      agent.TokenUsage

	toolIndex map[int]*chatToolCallBuilder
	toolOrder []int
}

type chatToolCallBuilder struct {
	id   string
	name string
	args strings.Builder
}

func (s *chatStreamAssembler) consume(r io.Reader) error {
	return scanSSE(r, s.handle)
}

func (s *chatStreamAssembler) handle(payload string) error {
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var ev chatStreamEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return fmt.Errorf("llm: decode stream event: %w", err)
	}
	if len(ev.Choices) > 0 {
		delta := ev.Choices[0].Delta
		if delta.ReasoningContent != "" {
			s.reasoning.WriteString(delta.ReasoningContent)
		}
		if delta.Content != "" {
			s.text.WriteString(delta.Content)
			if s.emit != nil {
				s.emit(agent.TextDelta{Text: delta.Content})
			}
		}
		for _, tc := range delta.ToolCalls {
			b := s.builder(tc.Index)
			if tc.ID != "" {
				b.id = tc.ID
			}
			if tc.Function.Name != "" {
				b.name = tc.Function.Name
			}
			b.args.WriteString(tc.Function.Arguments)
		}
		if ev.Choices[0].FinishReason != "" {
			s.stopReason = ev.Choices[0].FinishReason
		}
	}
	if ev.Usage != nil {
		s.usage.Input = ev.Usage.PromptTokens
		s.usage.Output = ev.Usage.CompletionTokens
	}
	return nil
}

func (s *chatStreamAssembler) builder(idx int) *chatToolCallBuilder {
	if s.toolIndex == nil {
		s.toolIndex = map[int]*chatToolCallBuilder{}
	}
	b, ok := s.toolIndex[idx]
	if !ok {
		b = &chatToolCallBuilder{}
		s.toolIndex[idx] = b
		s.toolOrder = append(s.toolOrder, idx)
	}
	return b
}

func (s *chatStreamAssembler) result() agent.TurnResult {
	blocks := make([]agent.Block, 0, len(s.toolOrder)+2)
	if s.reasoning.Len() > 0 {
		blocks = append(blocks, agent.Block{Type: agent.BlockThinking, Text: s.reasoning.String()})
	}
	if s.text.Len() > 0 {
		blocks = append(blocks, agent.NewText(s.text.String()))
	}
	for _, idx := range s.toolOrder {
		b := s.toolIndex[idx]
		blocks = append(blocks, agent.NewToolUse(b.id, b.name, json.RawMessage(chatArgumentsString(b.args.String()))))
	}
	stopReason := s.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
	} else if stopReason == "tool_calls" {
		stopReason = "tool_use"
	}
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: blocks},
		StopReason: stopReason,
		Usage:      s.usage,
	}
}

// TurnStream 以 SSE 流式调用 Chat Completions(stream:true),边接收边发 text 增量。
func (c *ChatClient) TurnStream(ctx context.Context, r agent.TurnRequest, emit func(agent.TextDelta)) (agent.TurnResult, error) {
	body, err := json.Marshal(chatRequest{
		Model:           c.Model,
		Messages:        domainToChatMessages(r.System, r.Messages),
		Tools:           specsToChatTools(r.Tools),
		MaxTokens:       c.MaxTokens,
		Temperature:     c.Temperature,
		ReasoningEffort: c.ReasoningEffort,
		Stream:          true,
	})
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := openAIPost(ctx, c.HTTP, c.APIKey, c.BaseURL+"/v1/chat/completions", body)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return agent.TurnResult{}, openAIErrorFromResponse(resp)
	}
	s := &chatStreamAssembler{emit: emit}
	if err := s.consume(resp.Body); err != nil {
		return agent.TurnResult{}, err
	}
	return s.result(), nil
}
