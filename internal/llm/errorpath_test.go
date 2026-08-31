package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

// fixedServer 回放固定状态码与正文,不校验 stream 标志;
// 故意不注入 HTTP 客户端,顺带覆盖 Client.http() 的默认传输分支。
func fixedServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return New("k", ts.URL, "m", 100)
}

func TestTurnStreamHTTPErrorParsesBody(t *testing.T) {
	c := fixedServer(t, http.StatusServiceUnavailable, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	_, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	apiErr := apiErrOf(err)
	if apiErr == nil || apiErr.Type != "overloaded_error" || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want overloaded_error with status", err)
	}
}

func TestTurnStreamHTTPErrorPlainBody(t *testing.T) {
	c := fixedServer(t, http.StatusInternalServerError, "gateway melted")
	_, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	apiErr := apiErrOf(err)
	if apiErr == nil || apiErr.Type != "http_error" || apiErr.Message != "gateway melted" {
		t.Fatalf("err = %v, want http_error fallback", err)
	}
}

func TestTurnStreamTransportError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close() // 立即关闭:连接必被拒,覆盖 post 的传输错误分支
	c := New("k", url, "m", 100)
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil {
		t.Fatal("want transport error")
	}
}

func TestTurnStreamOversizedLine(t *testing.T) {
	huge := strings.Repeat("x", maxSSELine+1)
	c := fixedServer(t, http.StatusOK, "data: "+huge+"\n\n")
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil || !strings.Contains(err.Error(), "read stream") {
		t.Fatalf("err = %v, want read stream failure", err)
	}
}

func TestTurnStreamMalformedDataJSON(t *testing.T) {
	c := fixedServer(t, http.StatusOK, "data: {oops\n\n")
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil || !strings.Contains(err.Error(), "decode stream event") {
		t.Fatalf("err = %v, want decode failure", err)
	}
}

func TestTurnStreamUnknownDeltaType(t *testing.T) {
	body := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"from_the_future\"}}\n\n"
	c := fixedServer(t, http.StatusOK, body)
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil {
		t.Fatal("want unknown delta error")
	}
}

func TestTurnStreamNoiseIsIgnored(t *testing.T) {
	body := strings.Join([]string{
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`data: [DONE]`,
		``,
		`data: {"type":"message_stop"}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta"}`,
		``,
		`data: {"type":"content_block_delta"}`,
		``,
	}, "\n")
	c := fixedServer(t, http.StatusOK, body)
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	if err != nil || len(res.Assistant.Blocks) != 0 {
		t.Fatalf("res = %+v err = %v, want clean empty result", res, err)
	}
}

func TestTurnStreamNilContentBlock(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"content_block_start","index":0}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
	}, "\n")
	c := fixedServer(t, http.StatusOK, body)
	res, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil)
	if err != nil || len(res.Assistant.Blocks) != 0 {
		t.Fatalf("res = %+v err = %v, want no blocks", res, err)
	}
}

func TestTurnStreamErrorEventWithoutPayload(t *testing.T) {
	c := fixedServer(t, http.StatusOK, "data: {\"type\":\"error\"}\n\n")
	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err == nil || !strings.Contains(err.Error(), "stream error event") {
		t.Fatalf("err = %v, want bare stream error", err)
	}
}

func TestAPIErrorText(t *testing.T) {
	got := (&APIError{Type: "t", Message: "m", Status: 500}).Error()
	if !strings.Contains(got, "t") || !strings.Contains(got, "m") || !strings.Contains(got, "500") {
		t.Fatalf("Error() = %q", got)
	}
}

func TestTurnUnexpectedBlockType(t *testing.T) {
	c := fixedServer(t, http.StatusOK, `{"role":"assistant","content":[{"type":"hologram","text":"?"}]}`)
	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err == nil || !strings.Contains(err.Error(), "unexpected content block type") {
		t.Fatalf("err = %v, want unexpected block type", err)
	}
}

func TestTurnThinkingBlockRoundTrip(t *testing.T) {
	c := fixedServer(t, http.StatusOK, `{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig1"},{"type":"text","text":"answer"}]}`)
	res, err := c.Turn(context.Background(), agent.TurnRequest{})
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
}
