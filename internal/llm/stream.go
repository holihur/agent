package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/holihur/agent/internal/agent"
)

// wireDelta 覆盖流式增量载荷的全部 delta 形状(按 type 区分)。
type wireDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`         // text_delta
	PartialJSON string `json:"partial_json,omitempty"` // input_json_delta
	Thinking    string `json:"thinking,omitempty"`     // thinking_delta
	Signature   string `json:"signature,omitempty"`    // signature_delta
	StopReason  string `json:"stop_reason,omitempty"`  // message_delta.delta
}

// wireStreamEvent 是 SSE data 行的事件信封(type 与 event 名一致)。
type wireStreamEvent struct {
	Type         string        `json:"type"`
	Error        *wireAPIError `json:"error,omitempty"`         // error 事件
	ContentBlock *wireBlock    `json:"content_block,omitempty"` // content_block_start
	Delta        *wireDelta    `json:"delta,omitempty"`         // content_block_delta / message_delta
}

// TurnStream 以 SSE 流式调用 /v1/messages(stream:true),边接收边把 text 增量
// 发给 emit(可为 nil),返回与非流式完全一致的最终 TurnResult。
// thinking 与 tool_use 的 JSON 增量静默组装,不进入 emit。
func (c *Client) TurnStream(ctx context.Context, r agent.TurnRequest, emit func(agent.TextDelta)) (agent.TurnResult, error) {
	body, err := json.Marshal(wireRequest{
		Model:           c.Model,
		MaxTokens:       c.MaxTokens,
		System:          r.System,
		Messages:        domainToWireMessages(r.Messages),
		Tools:           specsToWireTools(r.Tools),
		Stream:          true,
		Temperature:     c.Temperature,
		ReasoningEffort: c.ReasoningEffort,
	})
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := c.post(ctx, body)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return agent.TurnResult{}, fmt.Errorf("llm: read error response: %w", err)
		}
		if apiErr := parseErrorBody(raw); apiErr != nil {
			apiErr.Status = resp.StatusCode
			return agent.TurnResult{}, apiErr
		}
		return agent.TurnResult{}, &APIError{Type: "http_error", Message: string(raw), Status: resp.StatusCode}
	}

	s := &streamAssembler{emit: emit}
	if err := s.consume(resp.Body); err != nil {
		return agent.TurnResult{}, err
	}
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: s.blocks},
		StopReason: s.stopReason,
	}, nil
}

// streamAssembler 按 SSE 帧累积一个 message 的内容块。
type streamAssembler struct {
	emit func(agent.TextDelta)

	blocks     []agent.Block
	stopReason string

	curType        string
	curText        strings.Builder
	curSig         strings.Builder
	curInput       strings.Builder
	curID, curName string
}

// consume 逐行解析 SSE:data 行累积,空行分发给事件处理;event:/注释行忽略。
func (s *streamAssembler) consume(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	var dataLines []string
	dispatch := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		return s.handle(payload)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(line[5:]))
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("llm: read stream: %w", err)
	}
	return dispatch() // 流末尾未跟空行时补分发
}

const maxSSELine = 1 << 20

func (s *streamAssembler) handle(payload string) error {
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var ev wireStreamEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return fmt.Errorf("llm: decode stream event: %w", err)
	}
	switch ev.Type {
	case "message_start", "ping", "message_stop":
	case "content_block_start":
		s.startBlock(ev.ContentBlock)
	case "content_block_delta":
		return s.applyDelta(ev.Delta)
	case "content_block_stop":
		s.finalizeBlock()
	case "message_delta":
		if ev.Delta != nil {
			s.stopReason = ev.Delta.StopReason
		}
	case "error":
		if ev.Error != nil {
			return &APIError{Type: ev.Error.Type, Message: ev.Error.Message}
		}
		return fmt.Errorf("llm: stream error event")
	default:
		return fmt.Errorf("llm: unexpected stream event type %q", ev.Type)
	}
	return nil
}

func (s *streamAssembler) startBlock(w *wireBlock) {
	if w == nil {
		return
	}
	s.curType = w.Type
	s.curText.Reset()
	s.curSig.Reset()
	s.curInput.Reset()
	s.curID, s.curName = w.ID, w.Name
	// tool_use 的 content_block_start.input 恒为 {} 占位,
	// 真实参数只经 input_json_delta 到达,故不在此播种。
}

func (s *streamAssembler) applyDelta(d *wireDelta) error {
	if d == nil {
		return nil
	}
	switch d.Type {
	case "text_delta":
		s.curText.WriteString(d.Text)
		if s.emit != nil {
			s.emit(agent.TextDelta{Text: d.Text})
		}
	case "input_json_delta":
		s.curInput.WriteString(d.PartialJSON)
	case "thinking_delta":
		s.curText.WriteString(d.Thinking)
	case "signature_delta":
		s.curSig.WriteString(d.Signature)
	default:
		return fmt.Errorf("llm: unexpected delta type %q", d.Type)
	}
	return nil
}

// finalize 把当前累积块落入 blocks(content_block_stop 时调用)。
func (s *streamAssembler) finalizeBlock() {
	switch s.curType {
	case "text":
		s.blocks = append(s.blocks, agent.NewText(s.curText.String()))
	case "thinking":
		s.blocks = append(s.blocks, agent.Block{
			Type: agent.BlockThinking, Text: s.curText.String(), Signature: s.curSig.String(),
		})
	case "tool_use":
		input := s.curInput.String()
		if strings.TrimSpace(input) == "" {
			input = "{}" // 协议要点 #4:入参必须是对象
		}
		s.blocks = append(s.blocks, agent.NewToolUse(s.curID, s.curName, json.RawMessage(input)))
	}
	s.curType = ""
}
