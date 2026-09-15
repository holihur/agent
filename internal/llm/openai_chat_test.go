package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

func newChatServer(t *testing.T, check func(t *testing.T, req chatRequest), respond func() (int, []byte)) *ChatClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer k" {
			t.Errorf("auth = %q", r.Header.Get("authorization"))
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		check(t, req)
		status, body := respond()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	c := NewChat("k", ts.URL, "test-model", 100)
	c.HTTP = ts.Client()
	return c
}

func TestNewChatMaxTokensDefault(t *testing.T) {
	if c := NewChat("k", "u", "m", 0); c.MaxTokens != defaultMaxTokens {
		t.Fatalf("got %d", c.MaxTokens)
	}
	if c := NewChat("k", "u", "m", -1); c.MaxTokens != defaultMaxTokens {
		t.Fatalf("got %d", c.MaxTokens)
	}
	if c := NewChat("k", "u", "m", 2048); c.MaxTokens != 2048 {
		t.Fatalf("got %d", c.MaxTokens)
	}
}

func TestChatTurnHappyPathAndMapping(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {
			if req.Model != "test-model" || req.MaxTokens != 100 {
				t.Errorf("model/max_tokens = %q/%d", req.Model, req.MaxTokens)
			}
			if len(req.Messages) != 4 {
				t.Fatalf("messages = %d", len(req.Messages))
			}
			if req.Messages[0].Role != "system" || req.Messages[0].Content != "be brief" {
				t.Errorf("system = %+v", req.Messages[0])
			}
			if req.Messages[1].Role != "user" || req.Messages[1].Content != "hi" {
				t.Errorf("user = %+v", req.Messages[1])
			}
			asst := req.Messages[2]
			if asst.Role != "assistant" || asst.Content != "thinking" || len(asst.ToolCalls) != 1 {
				t.Fatalf("assistant = %+v", asst)
			}
			if asst.ToolCalls[0].Function.Name != "get_time" || asst.ToolCalls[0].Function.Arguments != `{"x":1}` {
				t.Errorf("tool call = %+v", asst.ToolCalls[0])
			}
			tool := req.Messages[3]
			if tool.Role != "tool" || tool.ToolCallID != "tu_1" || tool.Content != "now" {
				t.Fatalf("tool = %+v", tool)
			}
		},
		func() (int, []byte) {
			return http.StatusOK, mustJSON(map[string]any{
				"id": "chatcmpl_1",
				"choices": []map[string]any{{
					"message":       map[string]any{"role": "assistant", "content": "hello"},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13},
			})
		})

	res, err := c.Turn(context.Background(), agent.TurnRequest{
		System: "be brief",
		Tools:  []agent.ToolSpec{{Name: "get_time", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		Messages: []agent.Message{
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("hi")}},
			{Role: agent.RoleAssistant, Blocks: []agent.Block{
				agent.NewText("thinking"),
				agent.NewToolUse("tu_1", "get_time", json.RawMessage(`{"x":1}`)),
			}},
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewToolResult("tu_1", "now", false)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "stop" {
		t.Errorf("stop_reason = %q", res.StopReason)
	}
	if res.Assistant.TextContent() != "hello" {
		t.Errorf("content = %q", res.Assistant.TextContent())
	}
	if res.Usage.Input != 10 || res.Usage.Output != 3 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestChatTurnToolUseMapping(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) {
			return http.StatusOK, mustJSON(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []map[string]any{{
							"id": "call_1", "type": "function",
							"function": map[string]any{"name": "shell", "arguments": `{"cmd":"ls"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		})
	res, err := c.Turn(context.Background(), agent.TurnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", res.StopReason)
	}
	b := res.Assistant.Blocks[0]
	if b.Type != agent.BlockToolUse || b.ID != "call_1" || b.Name != "shell" || string(b.Input) != `{"cmd":"ls"}` {
		t.Errorf("tool_use = %+v", b)
	}
}

func TestChatTurnEmptyFinishReasonDefaultsEndTurn(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) {
			return http.StatusOK, []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		})
	res, err := c.Turn(context.Background(), agent.TurnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", res.StopReason)
	}
}

func TestChatTurnErrorBody(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) {
			return http.StatusBadRequest, []byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`)
		})
	_, err := c.Turn(context.Background(), agent.TurnRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Type != "invalid_request_error" || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTurnPlainHTTPError(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) { return http.StatusInternalServerError, []byte(`melted`) })
	_, err := c.Turn(context.Background(), agent.TurnRequest{})
	apiErr := apiErrOf(err)
	if apiErr == nil || apiErr.Type != "http_error" || apiErr.Message != "melted" {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTurn2xxErrorBody(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) { return http.StatusOK, []byte(`{"error":{"message":"m","type":"t"}}`) })
	_, err := c.Turn(context.Background(), agent.TurnRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Type != "t" {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTurnNoChoices(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) { return http.StatusOK, []byte(`{"choices":[]}`) })
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTurnDecodeError(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) { return http.StatusOK, []byte(`{oops`) })
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTurnBaseURLRequired(t *testing.T) {
	c := NewChat("k", "", "m", 100)
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTemperatureAndReasoningEffortSent(t *testing.T) {
	captured := map[string]any{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer ts.Close()
	c := NewChat("k", ts.URL, "m", 100)
	c.HTTP = ts.Client()
	temp := 0.3
	c.Temperature = &temp
	c.ReasoningEffort = "high"
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, ok := captured["temperature"].(float64); !ok || got != 0.3 {
		t.Fatalf("temperature = %v", captured["temperature"])
	}
	if got, ok := captured["reasoning_effort"].(string); !ok || got != "high" {
		t.Fatalf("reasoning_effort = %v", captured["reasoning_effort"])
	}
}

func TestDomainToChatMessages(t *testing.T) {
	out := domainToChatMessages("sys", []agent.Message{
		{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("a"), agent.NewText("b")}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{{Type: agent.BlockThinking, Text: "hmm"}}},
		{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewToolResult("t1", "x", false), agent.NewText("tail")}},
	})
	if len(out) != 5 {
		t.Fatalf("messages = %+v", out)
	}
	if out[0].Role != "system" || out[0].Content != "sys" {
		t.Errorf("system = %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "a\nb" {
		t.Errorf("user = %+v", out[1])
	}
	// 思考-only 的 assistant 消息保留 reasoning_content(DeepSeek 带 tools 时强制回传)。
	if out[2].Role != "assistant" || out[2].ReasoningContent != "hmm" || out[2].Content != "" || len(out[2].ToolCalls) != 0 {
		t.Errorf("thinking assistant = %+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "t1" || out[3].Content != "x" {
		t.Errorf("tool = %+v", out[3])
	}
	if out[4].Role != "user" || out[4].Content != "tail" {
		t.Errorf("tail = %+v", out[4])
	}
}

func TestSpecsToChatTools(t *testing.T) {
	if specsToChatTools(nil) != nil {
		t.Fatal("nil specs must stay nil")
	}
	got := specsToChatTools([]agent.ToolSpec{{Name: "n", Description: "d", InputSchema: map[string]any{"type": "object"}}})
	if len(got) != 1 || got[0].Type != "function" || got[0].Function.Name != "n" || got[0].Function.Description != "d" {
		t.Fatalf("tools = %+v", got)
	}
}

func TestRawContentText(t *testing.T) {
	if rawContentText(json.RawMessage(`"hi"`)) != "hi" {
		t.Fatal("string content")
	}
	if rawContentText(json.RawMessage(`null`)) != "" {
		t.Fatal("null content")
	}
	if rawContentText(json.RawMessage(`[{"type":"text","text":"x"}]`)) != "" {
		t.Fatal("array content")
	}
	if rawContentText(nil) != "" {
		t.Fatal("nil content")
	}
}

func chatSSEServer(t *testing.T, body string) *ChatClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Errorf("request must carry stream:true")
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	c := NewChat("k", ts.URL, "m", 100)
	c.HTTP = ts.Client()
	return c
}

func TestChatTurnStreamAssemblesAndEmits(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" world","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	c := chatSSEServer(t, body)

	var deltas []string
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, func(d agent.TextDelta) {
		deltas = append(deltas, d.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", res.StopReason)
	}
	if strings.Join(deltas, "|") != "Hello| world" {
		t.Errorf("deltas = %v", deltas)
	}
	if len(res.Assistant.Blocks) != 2 {
		t.Fatalf("blocks = %+v", res.Assistant.Blocks)
	}
	if res.Assistant.Blocks[0].Text != "Hello world" {
		t.Errorf("text = %q", res.Assistant.Blocks[0].Text)
	}
	tb := res.Assistant.Blocks[1]
	if tb.Type != agent.BlockToolUse || tb.ID != "call_1" || tb.Name != "shell" || string(tb.Input) != `{"cmd":"ls"}` {
		t.Errorf("tool_use = %+v", tb)
	}
	if res.Usage.Input != 5 || res.Usage.Output != 7 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestChatTurnStreamHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"busy","type":"overloaded"}}`))
	}))
	defer ts.Close()
	c := NewChat("k", ts.URL, "m", 100)
	c.HTTP = ts.Client()
	_, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	apiErr := apiErrOf(err)
	if apiErr == nil || apiErr.Type != "overloaded" {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTurnStreamEmptyResult(t *testing.T) {
	c := chatSSEServer(t, "data: [DONE]\n\n")
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "end_turn" || len(res.Assistant.Blocks) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestChatTurnStreamMalformed(t *testing.T) {
	c := chatSSEServer(t, "data: {oops\n\n")
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil || !strings.Contains(err.Error(), "decode stream event") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatTurnStreamTransportError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()
	c := NewChat("k", url, "m", 100)
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil {
		t.Fatal("want transport error")
	}
}

func TestChatReasoningContentMappingAndRoundTrip(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) {
			return http.StatusOK, []byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"hmm","content":"answer"},"finish_reason":"stop"}]}`)
		})
	res, err := c.Turn(context.Background(), agent.TurnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Assistant.Blocks) != 2 {
		t.Fatalf("blocks = %+v", res.Assistant.Blocks)
	}
	if res.Assistant.Blocks[0].Type != agent.BlockThinking || res.Assistant.Blocks[0].Text != "hmm" {
		t.Errorf("thinking = %+v", res.Assistant.Blocks[0])
	}
	if res.Assistant.Blocks[1].Type != agent.BlockText || res.Assistant.Blocks[1].Text != "answer" {
		t.Errorf("text = %+v", res.Assistant.Blocks[1])
	}

	// 回传:thinking 块必须重建为 reasoning_content(DeepSeek 带 tools 时要求)。
	out := domainToChatMessages("", []agent.Message{res.Assistant})
	if len(out) != 1 || out[0].ReasoningContent != "hmm" || out[0].Content != "answer" {
		t.Fatalf("round-trip = %+v", out)
	}
}

func TestChatReasoningOnlyFallsBackToThinking(t *testing.T) {
	c := newChatServer(t,
		func(t *testing.T, req chatRequest) {},
		func() (int, []byte) {
			return http.StatusOK, []byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"thinking only"},"finish_reason":"stop"}]}`)
		})
	res, err := c.Turn(context.Background(), agent.TurnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Assistant.TextContent(); got != "thinking only" {
		t.Fatalf("TextContent = %q, want reasoning fallback", got)
	}
}

func TestChatTurnStreamReasoningContent(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"hmm"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	c := chatSSEServer(t, body)

	var deltas []string
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, func(d agent.TextDelta) {
		deltas = append(deltas, d.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	// thinking 不 emit,只 emit 最终 content。
	if strings.Join(deltas, "|") != "answer" {
		t.Errorf("deltas = %v", deltas)
	}
	if len(res.Assistant.Blocks) != 2 {
		t.Fatalf("blocks = %+v", res.Assistant.Blocks)
	}
	if res.Assistant.Blocks[0].Type != agent.BlockThinking || res.Assistant.Blocks[0].Text != "hmm" {
		t.Errorf("thinking = %+v", res.Assistant.Blocks[0])
	}
	if res.Assistant.Blocks[1].Type != agent.BlockText || res.Assistant.Blocks[1].Text != "answer" {
		t.Errorf("text = %+v", res.Assistant.Blocks[1])
	}
}
