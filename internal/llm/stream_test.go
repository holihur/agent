package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent/internal/agent"
)

// sseServer 起一个假 /v1/messages,断言请求带 stream:true,回放给定 SSE 正文。
func sseServer(t *testing.T, body string) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Errorf("request must carry stream:true")
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	c := New("k", ts.URL, "m", 100)
	c.HTTP = ts.Client()
	return c
}

func TestTurnStreamAssemblesBlocksAndEmitsText(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[]}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"calculator","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"expr\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"3+5\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	c := sseServer(t, body)

	var deltas []string
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, func(d agent.TextDelta) {
		deltas = append(deltas, d.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", res.StopReason)
	}
	if len(res.Assistant.Blocks) != 2 {
		t.Fatalf("blocks = %+v", res.Assistant.Blocks)
	}
	if got := res.Assistant.Blocks[0].Text; got != "Hello world" {
		t.Fatalf("text = %q", got)
	}
	tb := res.Assistant.Blocks[1]
	if tb.Type != agent.BlockToolUse || tb.ID != "tu_1" || tb.Name != "calculator" {
		t.Fatalf("tool_use = %+v", tb)
	}
	if string(tb.Input) != `{"expr":"3+5"}` {
		t.Fatalf("input = %s", tb.Input)
	}
	if strings.Join(deltas, "|") != "Hello| world" {
		t.Fatalf("deltas = %v", deltas)
	}
}

func TestTurnStreamThinkingAssembledSilently(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig1"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		``,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
	}, "\n")
	c := sseServer(t, body)

	var deltas []string
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, func(d agent.TextDelta) { deltas = append(deltas, d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Assistant.Blocks) != 2 {
		t.Fatalf("blocks = %+v", res.Assistant.Blocks)
	}
	th := res.Assistant.Blocks[0]
	if th.Type != agent.BlockThinking || th.Text != "hmm" || th.Signature != "sig1" {
		t.Fatalf("thinking = %+v", th)
	}
	if len(deltas) != 1 || deltas[0] != "answer" {
		t.Fatalf("deltas = %v (thinking must not emit)", deltas)
	}
	if res.Assistant.TextContent() != "answer" {
		t.Fatalf("TextContent = %q", res.Assistant.TextContent())
	}
}

func TestTurnStreamMidStreamError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{}}`,
		``,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"try again"}}`,
		``,
	}, "\n")
	c := sseServer(t, body)
	_, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	if apiErrOf(err) == nil || apiErrOf(err).Type != "overloaded_error" {
		t.Fatalf("err = %v, want APIError(overloaded_error)", err)
	}
}

func TestTurnStreamUnknownEventFailsLoud(t *testing.T) {
	body := "data: {\"type\":\"from_the_future\"}\n\n"
	c := sseServer(t, body)
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil {
		t.Fatal("expected unknown event error")
	}
}

func apiErrOf(err error) *APIError {
	if e, ok := err.(*APIError); ok {
		return e
	}
	return nil
}
