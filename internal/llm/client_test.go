package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent/internal/agent"
)

// newTestServer 起一个假 /v1/messages,把收到的请求解码后交给 check,
// 然后按 respond 返回响应(或以 status 返回错误体)。
func newTestServer(t *testing.T, check func(t *testing.T, req wireRequest), respond func() (int, []byte)) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "k-test" {
			t.Errorf("missing x-api-key")
		}
		if r.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("anthropic-version = %q", r.Header.Get("anthropic-version"))
		}
		var req wireRequest
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
	c := New("k-test", ts.URL, "test-model", 100)
	c.AuthStyle = "both"
	c.HTTP = ts.Client()
	return c
}

func TestTurnHappyPathShapeAndMapping(t *testing.T) {
	c := newTestServer(t,
		func(t *testing.T, req wireRequest) {
			// 协议要点 #1/#2:system 顶层、max_tokens 必填。
			if req.System != "be brief" {
				t.Errorf("system = %q", req.System)
			}
			if req.MaxTokens != 100 {
				t.Errorf("max_tokens = %d", req.MaxTokens)
			}
			if req.Model != "test-model" {
				t.Errorf("model = %q", req.Model)
			}
			if len(req.Tools) != 1 || req.Tools[0].Name != "get_time" || req.Tools[0].InputSchema == nil {
				t.Errorf("tools = %+v", req.Tools)
			}
			// 协议要点 #4:空入参 tool_use 必须序列化为 {}。
			if len(req.Messages) < 2 {
				t.Fatalf("messages = %d", len(req.Messages))
			}
			backfill := req.Messages[1]
			if backfill.Role != "user" || backfill.Content[0].Type != "tool_result" ||
				backfill.Content[0].ToolUseID != "tu_1" || !backfill.Content[0].IsError {
				t.Errorf("backfill message = %+v", backfill)
			}
			if req.Messages[0].Content[0].Type != "text" {
				t.Errorf("first message = %+v", req.Messages[0])
			}
		},
		func() (int, []byte) {
			return http.StatusOK, mustJSON(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant",
				"content":     []map[string]any{{"type": "text", "text": "hello"}},
				"stop_reason": "end_turn",
			})
		})

	res, err := c.Turn(context.Background(), agent.TurnRequest{
		System: "be brief",
		Tools:  []agent.ToolSpec{{Name: "get_time", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		Messages: []agent.Message{
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("hi")}},
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewToolResult("tu_1", "error: boom", true)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", res.StopReason)
	}
	if got := res.Assistant.TextContent(); got != "hello" {
		t.Errorf("content = %q", got)
	}
	if res.Assistant.Role != agent.RoleAssistant {
		t.Errorf("role = %q", res.Assistant.Role)
	}
}

func TestTurnToolUseMapping(t *testing.T) {
	c := newTestServer(t,
		func(t *testing.T, req wireRequest) {
			// assistant 历史里的 tool_use 必须带 input 对象。
			asst := req.Messages[1]
			if asst.Role != "assistant" || asst.Content[0].Type != "tool_use" {
				t.Fatalf("assistant echo = %+v", asst)
			}
			if string(asst.Content[0].Input) != `{"expr":"1/0"}` {
				t.Errorf("input = %s", asst.Content[0].Input)
			}
		},
		func() (int, []byte) {
			return http.StatusOK, mustJSON(map[string]any{
				"id": "msg_2", "type": "message", "role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "tu_2", "name": "calculator", "input": map[string]any{"expr": "1/0"}},
				},
				"stop_reason": "tool_use",
			})
		})

	res, err := c.Turn(context.Background(), agent.TurnRequest{
		Messages: []agent.Message{
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("hi")}},
			{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewToolUse("tu_x", "calculator", json.RawMessage(`{"expr":"1/0"}`))}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", res.StopReason)
	}
	b := res.Assistant.Blocks[0]
	if b.Type != agent.BlockToolUse || b.ID != "tu_2" || b.Name != "calculator" || string(b.Input) != `{"expr":"1/0"}` {
		t.Errorf("tool_use block = %+v", b)
	}
}

func TestTurnEmptyInputSerializedAsObject(t *testing.T) {
	c := newTestServer(t,
		func(t *testing.T, req wireRequest) {
			asst := req.Messages[1]
			if string(asst.Content[0].Input) != "{}" {
				t.Errorf("empty input = %s, want {}", asst.Content[0].Input)
			}
		},
		func() (int, []byte) {
			return http.StatusOK, mustJSON(map[string]any{
				"id": "msg_3", "type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "text", "text": "ok"}}, "stop_reason": "end_turn",
			})
		})
	_, err := c.Turn(context.Background(), agent.TurnRequest{
		Messages: []agent.Message{
			{Role: agent.RoleUser, Blocks: []agent.Block{agent.NewText("hi")}},
			{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewToolUse("tu_3", "get_time", nil)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTurnAPIErrorBody(t *testing.T) {
	c := newTestServer(t,
		func(t *testing.T, _ wireRequest) {},
		func() (int, []byte) {
			return http.StatusBadRequest, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad tool name"}}`)
		})
	_, err := c.Turn(context.Background(), agent.TurnRequest{Messages: []agent.Message{}}) //nolint:staticcheck // 空消息即触发 4xx
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Type != "invalid_request_error" || apiErr.Status != http.StatusBadRequest {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestTurnUnknownBlockTypeFailsLoud(t *testing.T) {
	c := newTestServer(t,
		func(t *testing.T, _ wireRequest) {},
		func() (int, []byte) {
			return http.StatusOK, []byte(`{"type":"message","role":"assistant","content":[{"type":"redacted_thinking","data":"..."}],"stop_reason":"end_turn"}`)
		})
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err == nil { //nolint:staticcheck // 故意空请求
		t.Fatal("expected error for unexpected block type")
	}
}

func TestAuthStyles(t *testing.T) {
	cases := []struct {
		style    string
		wantXKey string
		wantAuth string
	}{
		{"", "", "Bearer k"},       // 默认 bearer(网关形态)
		{"bearer", "", "Bearer k"}, //nolint:staticcheck // 显式覆盖默认值本身即用例
		{"x-api-key", "k", ""},     //nolint:staticcheck
		{"both", "k", "Bearer k"},  //nolint:staticcheck
	}
	for _, tc := range cases {
		var gotXKey, gotAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotXKey = r.Header.Get("x-api-key")
			gotAuth = r.Header.Get("authorization")
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
		}))
		c := New("k", ts.URL, "m", 10)
		c.AuthStyle = tc.style
		c.HTTP = ts.Client()
		if _, err := c.Turn(context.Background(), agent.TurnRequest{Messages: []agent.Message{}}); err != nil { //nolint:staticcheck // 空消息即可触发请求
			t.Fatalf("%q: %v", tc.style, err)
		}
		ts.Close()
		if gotXKey != tc.wantXKey || gotAuth != tc.wantAuth {
			t.Errorf("style %q: x-api-key=%q auth=%q, want (%q, %q)", tc.style, gotXKey, gotAuth, tc.wantXKey, tc.wantAuth)
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
