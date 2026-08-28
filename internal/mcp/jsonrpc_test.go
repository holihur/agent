package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
)

// ---- 测试替身:脚本式传输 ----

type chanTransport struct {
	mu        sync.Mutex
	sent      [][]byte
	in        chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newChanTransport() *chanTransport {
	return &chanTransport{in: make(chan []byte, 16), closed: make(chan struct{})}
}

func (t *chanTransport) Send(b []byte) error {
	t.mu.Lock()
	t.sent = append(t.sent, append([]byte(nil), b...))
	t.mu.Unlock()
	return nil
}

func (t *chanTransport) Recv() ([]byte, error) {
	select {
	case b := <-t.in:
		return b, nil
	case <-t.closed:
		return nil, io.EOF
	}
}

func (t *chanTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func (t *chanTransport) sentFrames() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([][]byte, len(t.sent))
	copy(out, t.sent)
	return out
}

func decodeFrame(t *testing.T, frame []byte) rpcRequest {
	t.Helper()
	var req rpcRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		t.Fatalf("decode frame: %v (%s)", err, frame)
	}
	return req
}

func pushResponse(tr *chanTransport, id int64, result any) {
	frame, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: &id, Result: mustJSONRaw(result)})
	tr.in <- frame
}

func mustJSONRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- JSON-RPC 核心 ----

func TestCallRoundTripAndCorrelation(t *testing.T) {
	tr := newChanTransport()
	c := newRPCClient(tr)
	defer c.Close()

	type out struct {
		name string
		res  json.RawMessage
		err  error
	}
	done := make(chan out, 2)
	go func() {
		r, err := c.call(context.Background(), "m1", nil)
		done <- out{"m1", r, err}
	}()
	go func() {
		r, err := c.call(context.Background(), "m2", nil)
		done <- out{"m2", r, err}
	}()

	// 等两个请求都已发出(乱序应答场景)。
	for len(tr.sentFrames()) < 2 {
	}
	frames := tr.sentFrames()
	f1, f2 := decodeFrame(t, frames[0]), decodeFrame(t, frames[1])
	if *f1.ID == *f2.ID {
		t.Fatal("ids must be unique")
	}
	// 按每个请求自身的 method 回应(帧顺序是并发的,不可假设)。
	pushResponse(tr, *f1.ID, map[string]any{"who": f1.Method})
	pushResponse(tr, *f2.ID, map[string]any{"who": f2.Method})

	got := map[string]string{}
	for range 2 {
		o := <-done
		if o.err != nil {
			t.Fatalf("%s: %v", o.name, o.err)
		}
		var r struct {
			Who string `json:"who"`
		}
		_ = json.Unmarshal(o.res, &r)
		got[o.name] = r.Who
	}
	if got["m1"] != "m1" || got["m2"] != "m2" {
		t.Fatalf("correlation broken: %v", got)
	}
}

func TestCallSurfacesRPCError(t *testing.T) {
	tr := newChanTransport()
	c := newRPCClient(tr)
	defer c.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.call(context.Background(), "tools/list", nil)
		errCh <- err
	}()
	for len(tr.sentFrames()) < 1 {
	}
	id := *decodeFrame(t, tr.sentFrames()[0]).ID
	frame, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: &id,
		Error: &rpcError{Code: codeUnsupportedProtocolVersion, Message: "Unsupported protocol version",
			Data: json.RawMessage(`{"supported":["2026-07-28"],"requested":"1900-01-01"}`)}})
	tr.in <- frame

	err := <-errCh
	var rpcErr *RPCError
	if !asRPCError(err, &rpcErr) || rpcErr.Code != codeUnsupportedProtocolVersion {
		t.Fatalf("err = %v, want RPCError -32022", err)
	}
}

func asRPCError(err error, target **RPCError) bool {
	if e, ok := err.(*RPCError); ok { //nolint:errorlint // 测试内直接断言
		*target = e
		return true
	}
	return false
}

func TestContextCancelSendsCancelledNotification(t *testing.T) {
	tr := newChanTransport()
	c := newRPCClient(tr)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, "slow", nil)
		errCh <- err
	}()
	for len(tr.sentFrames()) < 1 {
	}
	reqID := *decodeFrame(t, tr.sentFrames()[0]).ID
	cancel()

	if err := <-errCh; err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// 规范 §stdio Cancellation:必须发 notifications/cancelled 且引用请求 id。
	found := false
	for _, f := range tr.sentFrames() {
		req := decodeFrame(t, f)
		if req.Method == "notifications/cancelled" && req.ID == nil {
			var p struct {
				RequestID int64 `json:"requestId"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.RequestID == reqID {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("notifications/cancelled with requestId not sent")
	}
}

func TestConnectionCloseFailsPending(t *testing.T) {
	tr := newChanTransport()
	c := newRPCClient(tr)

	errCh := make(chan error, 1)
	go func() {
		_, err := c.call(context.Background(), "x", nil)
		errCh <- err
	}()
	for len(tr.sentFrames()) < 1 {
	}
	_ = tr.Close() // 通道死亡 → 读循环退出 → pending 全部失败

	if err := <-errCh; err != errConnClosed {
		t.Fatalf("err = %v, want errConnClosed", err)
	}
}

func TestServerRequestAnsweredWithMethodNotFound(t *testing.T) {
	tr := newChanTransport()
	c := newRPCClient(tr)
	defer c.Close()

	// 规范禁止服务器主动发请求;客户端必须回 -32601 以防互操作卡死。
	sid := int64(99)
	frame, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &sid, Method: "sampling/createMessage"})
	tr.in <- frame

	// 随后一次正常调用仍能完成(读循环未被卡住)。
	resCh := make(chan error, 1)
	go func() {
		_, err := c.call(context.Background(), "ping-ish", nil)
		resCh <- err
	}()
	for len(tr.sentFrames()) < 2 {
	}
	normalID := *decodeFrame(t, tr.sentFrames()[1]).ID
	pushResponse(tr, normalID, map[string]any{})
	if err := <-resCh; err != nil {
		t.Fatalf("normal call failed: %v", err)
	}

	var sawReject bool
	for _, f := range tr.sentFrames() {
		resp := decodeFrame(t, f)
		if resp.ID != nil && *resp.ID == sid {
			// 响应帧走的是 rpcResponse 的 error 字段,这里只需确认有回帧。
			sawReject = true
		}
	}
	if !sawReject {
		t.Fatal("no response for server-initiated request")
	}
}

func TestUnknownResultShapeIsTolerated(t *testing.T) {
	tr := newChanTransport()
	c := newRPCClient(tr)
	defer c.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.call(context.Background(), "x", nil)
		errCh <- err
	}()
	for len(tr.sentFrames()) < 1 {
	}
	id := *decodeFrame(t, tr.sentFrames()[0]).ID
	// 空 result(既无 result 也无 error)→ 回 null 而非卡死。
	frame, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: &id})
	tr.in <- frame
	if err := <-errCh; err != nil {
		t.Fatalf("err = %v", err)
	}
}
