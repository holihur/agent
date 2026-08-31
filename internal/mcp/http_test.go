package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// replyJSON 组一个 JSON-RPC 响应体。
func replyJSON(id int64, result any) []byte {
	b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: &id, Result: mustJSONRaw(result)})
	return b
}

// replySSE 把响应帧包成 SSE 事件。
func replySSE(frame []byte) string {
	return fmt.Sprintf("event: message\ndata: %s\n\n", frame)
}

// echoHTTPHandler 实现最小现代 MCP 服务器(server/discover → tools/list → tools/call),
// 以 application/json 单帧应答;每次请求经 checked 通知测试断言请求头。
func echoHTTPHandler(t *testing.T, session string, checked func(req *http.Request)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, req *http.Request) {
		if checked != nil {
			checked(req)
		}
		var fr rpcRequest
		if err := json.NewDecoder(req.Body).Decode(&fr); err != nil || fr.ID == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch fr.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			if session != "" {
				w.Header().Set("Mcp-Session-Id", session)
			}
			w.Write(replyJSON(*fr.ID, map[string]any{"supportedVersions": []string{protocolVersion}}))
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			w.Write(replyJSON(*fr.ID, map[string]any{
				"resultType": "complete",
				"tools": []map[string]any{{
					"name": "echo", "description": "d",
					"inputSchema": map[string]any{"type": "object"},
				}},
				"nextCursor": "",
			}))
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			w.Write(replyJSON(*fr.ID, map[string]any{
				"resultType": "complete",
				"content":    []map[string]any{{"type": "text", "text": "pong"}},
				"isError":    false,
			}))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: fr.ID,
				Error: &rpcError{Code: codeMethodNotFound, Message: "nope"}})
			w.Write(b)
		}
	}
}

// TestHTTPProviderJSONRoundTrip 走真 Provider:探测 → 列表 → 调用,全经 HTTP。
func TestHTTPProviderJSONRoundTrip(t *testing.T) {
	srv := httptest.NewServer(echoHTTPHandler(t, "", nil))
	defer srv.Close()

	p := NewHTTP("remote", HTTPConfig{URL: srv.URL}, nil)
	defer p.Close()

	defs, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "echo" {
		t.Fatalf("defs = %+v", defs)
	}
	res, err := p.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "pong" || res.IsError {
		t.Fatalf("res = %+v", res)
	}
}

// TestHTTPEchoesSessionAndHeaders 断言必发头、自定义鉴权头与 Mcp-Session-Id 回显。
func TestHTTPEchoesSessionAndHeaders(t *testing.T) {
	var calls int
	srv := httptest.NewServer(echoHTTPHandler(t, "sess-42", func(req *http.Request) {
		calls++
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("call %d Content-Type = %q", calls, got)
		}
		if got := req.Header.Get("Accept"); !strings.Contains(got, "application/json") || !strings.Contains(got, "text/event-stream") {
			t.Errorf("call %d Accept = %q", calls, got)
		}
		if got := req.Header.Get("MCP-Protocol-Version"); got != protocolVersion {
			t.Errorf("call %d MCP-Protocol-Version = %q", calls, got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("call %d Authorization = %q", calls, got)
		}
		// 第 2 次起必须回显服务器指派的 session(规范 §transports)。
		if calls > 1 && req.Header.Get("Mcp-Session-Id") != "sess-42" {
			t.Errorf("call %d Mcp-Session-Id = %q, want sess-42", calls, req.Header.Get("Mcp-Session-Id"))
		}
	}))
	defer srv.Close()

	p := NewHTTP("remote", HTTPConfig{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer tok"}}, nil)
	defer p.Close()
	if _, err := p.ListTools(context.Background()); err != nil { // discover + list 两次 POST
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("calls = %d", calls)
	}
}

// TestHTTPSSEStreamRoundTrip 响应走 text/event-stream:先一条通知帧,再本请求的响应帧。
func TestHTTPSSEStreamRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var fr rpcRequest
		if json.NewDecoder(req.Body).Decode(&fr) != nil || fr.ID == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch fr.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(replySSE(replyJSON(*fr.ID, map[string]any{"supportedVersions": []string{protocolVersion}}))))
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			// 先推一条与本请求无关的通知(应被丢弃),再推响应。
			w.Write([]byte(replySSE([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"p":1}}`))))
			w.Write([]byte(replySSE(replyJSON(*fr.ID, map[string]any{
				"resultType": "complete",
				"tools": []map[string]any{{
					"name": "echo", "description": "d",
					"inputSchema": map[string]any{"type": "object"},
				}},
			}))))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	p := NewHTTP("remote", HTTPConfig{URL: srv.URL}, nil)
	defer p.Close()

	defs, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "echo" {
		t.Fatalf("defs = %+v", defs)
	}
}

// TestHTTPNotificationAccepted202 通知帧 → 202 无响应帧,Send 返回 nil。
func TestHTTPNotificationAccepted202(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var fr rpcRequest
		_ = json.NewDecoder(req.Body).Decode(&fr)
		if fr.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	tr, err := dialHTTP(context.Background(), HTTPConfig{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	frame, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/cancelled",
		Params: mustJSONRaw(map[string]any{"requestId": 1})})
	if err := tr.Send(frame); err != nil {
		t.Fatalf("notify send = %v", err)
	}
}

// TestHTTPRPCErrorSurfacesFromHTTPStatus 4xx + JSON-RPC 错误体 → 调用方拿到 RPCError。
func TestHTTPRPCErrorSurfacesFromHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var fr rpcRequest
		_ = json.NewDecoder(req.Body).Decode(&fr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: fr.ID,
			Error: &rpcError{Code: codeMethodNotFound, Message: "Method not found"}})
		w.Write(b)
	}))
	defer srv.Close()

	tr, err := dialHTTP(context.Background(), HTTPConfig{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	c := newRPCClient(tr)
	defer c.Close()
	_, err = c.call(context.Background(), "tools/list", nil)
	var rpcErr *RPCError
	if !asRPCError(err, &rpcErr) || rpcErr.Code != codeMethodNotFound {
		t.Fatalf("err = %v, want RPCError -32601", err)
	}
}

// TestHTTPNonJSONErrorBody HTML 错误页等非 JSON-RPC 体 → 状态码进错误信息。
func TestHTTPNonJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("<html>boom</html>"))
	}))
	defer srv.Close()

	tr, err := dialHTTP(context.Background(), HTTPConfig{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	frame, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: int64p(1), Method: "tools/list"})
	err = tr.Send(frame)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want status 500 in message", err)
	}
}

// TestHTTPCloseFailsInFlight 远端挂住时 Close 必须让在途请求失败,而非永久阻塞。
func TestHTTPCloseFailsInFlight(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	defer close(release)

	tr, err := dialHTTP(context.Background(), HTTPConfig{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	frame, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: int64p(1), Method: "tools/list"})
	errCh := make(chan error, 1)
	go func() { errCh <- tr.Send(frame) }()

	time.Sleep(50 * time.Millisecond) // 等 POST 进入挂起
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("want error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send still blocked 2s after Close")
	}
}

// TestDialHTTPRejectsBadURL 非 http(s) scheme 在建连期即拒绝。
func TestDialHTTPRejectsBadURL(t *testing.T) {
	if _, err := dialHTTP(context.Background(), HTTPConfig{URL: "ftp://x/mcp"}); err == nil {
		t.Fatal("want error for non-http scheme")
	}
	if _, err := dialHTTP(context.Background(), HTTPConfig{URL: ""}); err == nil {
		t.Fatal("want error for empty url")
	}
}

func int64p(v int64) *int64 { return &v }
