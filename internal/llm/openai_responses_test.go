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

func newResponsesServer(t *testing.T, check func(t *testing.T, req responsesRequest), respond func() (int, []byte)) *ResponsesClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer k" {
			t.Errorf("auth = %q", r.Header.Get("authorization"))
		}
		var req responsesRequest
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
	c := NewResponses("k", ts.URL, "test-model", 100)
	c.HTTP = ts.Client()
	return c
}

func TestNewResponsesMaxTokensDefault(t *testing.T) {
	if c := NewResponses("k", "u", "m", 0); c.MaxTokens != defaultMaxTokens {
		t.Fatalf("got %d", c.MaxTokens)
	}
	if c := NewResponses("k", "u", "m", -2); c.MaxTokens != defaultMaxTokens {
		t.Fatalf("got %d", c.MaxTokens)
	}
	if c := NewResponses("k", "u", "m", 2048); c.MaxTokens != 2048 {
		t.Fatalf("got %d", c.MaxTokens)
	}
}

func TestResponsesTurnHappyPathAndMapping(t *testing.T) {
	c := newResponsesServer(t,
		func(t *testing.T, req responsesRequest) {
			if req.Model != "test-model" || req.MaxOutputTokens != 100 {
				t.Errorf("model/max_output_tokens = %q/%d", req.Model, req.MaxOutputTokens)
			}
			if req.Instructions != "be brief" {
				t.Errorf("instructions = %q", req.Instructions)
			}
			if len(req.Input) != 3 {
				t.Fatalf("input = %+v", req.Input)
			}
			if req.Input[0].Role != "user" || req.Input[0].Content != "hi" {
				t.Errorf("input[0] = %+v", req.Input[0])
			}
			fc := req.Input[1]
			if fc.Type != "function_call" || fc.CallID != "tu_1" || fc.Name != "get_time" || fc.Arguments != `{"x":1}` {
				t.Errorf("function_call = %+v", fc)
			}
			fco := req.Input[2]
			if fco.Type != "function_call_output" || fco.CallID != "tu_1" || fco.Output != "now" {
				t.Errorf("function_call_output = %+v", fco)
			}
			if len(req.Tools) != 1 || req.Tools[0].Name != "get_time" {
				t.Errorf("tools = %+v", req.Tools)
			}
		},
		func() (int, []byte) {
			return http.StatusOK, mustJSON(map[string]any{
				"id": "resp_1", "status": "completed",
				"output": []map[string]any{
					{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "hello"}}},
				},
				"usage": map[string]any{
					"input_tokens": 10, "output_tokens": 3, "total_tokens": 13,
					"input_tokens_details": map[string]any{"cached_tokens": 2},
				},
			})
		})

	res, err := c.Turn(context.Background(), agent.TurnRequest{
		System: "be brief",
		Tools:  []agent.ToolSpec{{Name: "get_time", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		Messages: []agent.Message{
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("hi")}},
			{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewToolUse("tu_1", "get_time", json.RawMessage(`{"x":1}`))}},
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewToolResult("tu_1", "now", false)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", res.StopReason)
	}
	if res.Assistant.TextContent() != "hello" {
		t.Errorf("content = %q", res.Assistant.TextContent())
	}
	if res.Usage.Input != 10 || res.Usage.Output != 3 || res.Usage.CacheRead != 2 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestResponsesTurnToolUse(t *testing.T) {
	c := newResponsesServer(t,
		func(t *testing.T, req responsesRequest) {},
		func() (int, []byte) {
			return http.StatusOK, mustJSON(map[string]any{
				"id": "resp_2", "status": "completed",
				"output": []map[string]any{
					{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": `{"cmd":"ls"}`},
				},
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

func TestResponsesToResultSkipsReasoning(t *testing.T) {
	rr := responsesResponse{
		Output: []responsesOutputItem{
			{Type: "reasoning"},
			{Type: "message", Content: []responsesContentPart{{Type: "output_text", Text: "ans"}}},
			{Type: "message", Content: []responsesContentPart{{Type: "text", Text: "ans2"}}},
		},
	}
	u := responsesUsage{InputTokens: 10, OutputTokens: 2}
	u.InputTokensDetails.CachedTokens = 3
	rr.Usage = &u
	res, err := responsesToResult(rr)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Assistant.Blocks) != 2 || res.Assistant.TextContent() != "ans\nans2" {
		t.Fatalf("blocks = %+v", res.Assistant.Blocks)
	}
	if res.Usage.CacheRead != 3 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestResponsesTurnErrorBody(t *testing.T) {
	c := newResponsesServer(t,
		func(t *testing.T, req responsesRequest) {},
		func() (int, []byte) {
			return http.StatusBadRequest, []byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`)
		})
	_, err := c.Turn(context.Background(), agent.TurnRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Type != "invalid_request_error" || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesTurnPlainHTTPError(t *testing.T) {
	c := newResponsesServer(t,
		func(t *testing.T, req responsesRequest) {},
		func() (int, []byte) { return http.StatusInternalServerError, []byte(`melted`) })
	_, err := c.Turn(context.Background(), agent.TurnRequest{})
	apiErr := apiErrOf(err)
	if apiErr == nil || apiErr.Type != "http_error" || apiErr.Message != "melted" {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesTurn2xxErrorBody(t *testing.T) {
	c := newResponsesServer(t,
		func(t *testing.T, req responsesRequest) {},
		func() (int, []byte) { return http.StatusOK, []byte(`{"error":{"message":"m","type":"t"}}`) })
	_, err := c.Turn(context.Background(), agent.TurnRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Type != "t" {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesTurnDecodeError(t *testing.T) {
	c := newResponsesServer(t,
		func(t *testing.T, req responsesRequest) {},
		func() (int, []byte) { return http.StatusOK, []byte(`{oops`) })
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesTurnBaseURLRequired(t *testing.T) {
	c := NewResponses("k", "", "m", 100)
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesReasoningEffortAndTemperature(t *testing.T) {
	captured := map[string]any{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(`{"status":"completed","output":[]}`))
	}))
	defer ts.Close()
	c := NewResponses("k", ts.URL, "m", 100)
	c.HTTP = ts.Client()
	temp := 0.4
	c.Temperature = &temp
	c.ReasoningEffort = "low"
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, ok := captured["temperature"].(float64); !ok || got != 0.4 {
		t.Fatalf("temperature = %v", captured["temperature"])
	}
	if r, ok := captured["reasoning"].(map[string]any); !ok || r["effort"] != "low" {
		t.Fatalf("reasoning = %v", captured["reasoning"])
	}
}

func TestDomainToResponsesItems(t *testing.T) {
	system, items := domainToResponsesItems("sys", []agent.Message{
		{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("hi")}},
		{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText("thinking"), agent.NewToolUse("t1", "shell", json.RawMessage(`{"c":"ls"}`))}},
		{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewToolResult("t1", "out", true)}},
	})
	if system != "sys" {
		t.Errorf("system = %q", system)
	}
	if len(items) != 4 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Role != "user" || items[0].Content != "hi" {
		t.Errorf("items[0] = %+v", items[0])
	}
	if items[1].Role != "assistant" || items[1].Content != "thinking" {
		t.Errorf("items[1] = %+v", items[1])
	}
	if items[2].Type != "function_call" || items[2].CallID != "t1" || items[2].Name != "shell" || items[2].Arguments != `{"c":"ls"}` {
		t.Errorf("items[2] = %+v", items[2])
	}
	if items[3].Type != "function_call_output" || items[3].CallID != "t1" || items[3].Output != "out" {
		t.Errorf("items[3] = %+v", items[3])
	}
}

func TestSpecsToResponsesTools(t *testing.T) {
	if specsToResponsesTools(nil) != nil {
		t.Fatal("nil specs must stay nil")
	}
	got := specsToResponsesTools([]agent.ToolSpec{{Name: "n", Description: "d", InputSchema: map[string]any{"type": "object"}}})
	if len(got) != 1 || got[0].Type != "function" || got[0].Name != "n" || got[0].Description != "d" {
		t.Fatalf("tools = %+v", got)
	}
}

func TestResponsesReasoningPtr(t *testing.T) {
	if responsesReasoningPtr("") != nil {
		t.Fatal("empty must be nil")
	}
	got := responsesReasoningPtr("high")
	if got == nil || got.Effort != "high" {
		t.Fatalf("got = %+v", got)
	}
}

func responsesSSEServer(t *testing.T, body string) *ResponsesClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responsesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Errorf("request must carry stream:true")
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	c := NewResponses("k", ts.URL, "m", 100)
	c.HTTP = ts.Client()
	return c
}

func TestResponsesTurnStreamAssemblesAndEmits(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"Hello"}`,
		``,
		`data: {"type":"response.output_text.done","output_index":0,"text":"Hello"}`,
		``,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"shell","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"cmd\":\"ls\"}"}`,
		``,
		`data: {"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"cmd\":\"ls\"}"}`,
		``,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	c := responsesSSEServer(t, body)

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
	if strings.Join(deltas, "|") != "Hello" {
		t.Errorf("deltas = %v", deltas)
	}
	if len(res.Assistant.Blocks) != 2 {
		t.Fatalf("blocks = %+v", res.Assistant.Blocks)
	}
	if res.Assistant.Blocks[0].Text != "Hello" {
		t.Errorf("text = %q", res.Assistant.Blocks[0].Text)
	}
	tb := res.Assistant.Blocks[1]
	if tb.Type != agent.BlockToolUse || tb.ID != "call_1" || tb.Name != "shell" || string(tb.Input) != `{"cmd":"ls"}` {
		t.Errorf("tool_use = %+v", tb)
	}
	if res.Usage.Input != 10 || res.Usage.Output != 3 || res.Usage.CacheRead != 2 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestResponsesTurnStreamOutputItemDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_9","name":"fs","arguments":"{\"p\":1}"}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	c := responsesSSEServer(t, body)
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" || len(res.Assistant.Blocks) != 1 {
		t.Fatalf("res = %+v", res)
	}
	tb := res.Assistant.Blocks[0]
	if tb.ID != "call_9" || tb.Name != "fs" || string(tb.Input) != `{"p":1}` {
		t.Errorf("tool_use = %+v", tb)
	}
}

func TestResponsesTurnStreamFailed(t *testing.T) {
	c := responsesSSEServer(t, `data: {"type":"response.failed","response":{"error":{"message":"boom","type":"server_error"}}}`+"\n\n")
	_, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	apiErr := apiErrOf(err)
	if apiErr == nil || apiErr.Type != "server_error" {
		t.Fatalf("err = %v", err)
	}

	c2 := responsesSSEServer(t, `data: {"type":"response.failed","response":{"status":"failed"}}`+"\n\n")
	if _, err := c2.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil || !strings.Contains(err.Error(), "responses stream failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesTurnStreamEmptyResult(t *testing.T) {
	c := responsesSSEServer(t, "data: [DONE]\n\n")
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "end_turn" || len(res.Assistant.Blocks) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestResponsesTurnStreamMalformed(t *testing.T) {
	c := responsesSSEServer(t, "data: {oops\n\n")
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil || !strings.Contains(err.Error(), "decode stream event") {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesTurnStreamHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"gw","type":"gateway_error"}}`))
	}))
	defer ts.Close()
	c := NewResponses("k", ts.URL, "m", 100)
	c.HTTP = ts.Client()
	_, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	apiErr := apiErrOf(err)
	if apiErr == nil || apiErr.Type != "gateway_error" {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesTurnStreamTransportError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()
	c := NewResponses("k", url, "m", 100)
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil {
		t.Fatal("want transport error")
	}
}
