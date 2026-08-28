package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// JSON-RPC 2.0 标准错误码 + MCP 规范保留段
// (规范 §basic/index 错误码表:-32020..-32099 由 MCP 规范独占)。
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603

	// UnsupportedProtocolVersionError:服务器不支持请求的协议版本。
	codeUnsupportedProtocolVersion = -32022
)

var errConnClosed = errors.New("mcp: connection closed")

// RPCError 是调用方可见的远端 JSON-RPC 错误(带码,便于识别 -32022 等)。
type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"` // nil = notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

// rpcClient 是 JSON-RPC 2.0 客户端核心,与 MCP 语义无关:
// ID 关联、读 goroutine 分发、取消通知、连接死亡传播。
type rpcClient struct {
	tr Transport

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcReply
	once    sync.Once
	done    chan struct{}
}

func newRPCClient(tr Transport) *rpcClient {
	c := &rpcClient{tr: tr, pending: map[int64]chan rpcReply{}, done: make(chan struct{})}
	go c.readLoop()
	return c
}

// readLoop 唯一的读者:按帧分发(响应 → pending;通知 → 丢弃;
// 服务器请求 → 礼貌拒绝)。2026-07-28 下服务器不主动发请求,
// 但响应可保证互操作(规范 §transports:客户端不发响应)。
func (c *rpcClient) readLoop() {
	for {
		frame, err := c.tr.Recv()
		if err != nil {
			c.shutdown()
			return
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(frame, &msg); err != nil {
			continue // 无法解析的帧无法关联回调,丢弃
		}
		switch {
		case msg.Method != "" && msg.ID != nil:
			// 规范禁止服务器主动发请求;回 method-not-found 以防互操作卡死。
			resp := rpcResponse{JSONRPC: "2.0", ID: msg.ID,
				Error: &rpcError{Code: codeMethodNotFound,
					Message: "client does not accept server-initiated requests"}}
			if b, err := json.Marshal(resp); err == nil {
				_ = c.tr.Send(b)
			}
		case msg.Method != "":
			// 通知(notifications/progress、list_changed 等):本客户端不订阅,忽略。
		case msg.ID != nil:
			ch := c.takePending(*msg.ID)
			if ch == nil {
				continue // 未知 id(可能已超时/取消):丢弃
			}
			switch {
			case msg.Error != nil:
				ch <- rpcReply{err: &RPCError{Code: msg.Error.Code, Message: msg.Error.Message, Data: msg.Error.Data}}
			case msg.Result != nil:
				ch <- rpcReply{result: msg.Result}
			default:
				ch <- rpcReply{result: json.RawMessage("null")}
			}
		}
	}
}

func (c *rpcClient) takePending(id int64) chan rpcReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.pending[id]
	delete(c.pending, id)
	return ch
}

func (c *rpcClient) shutdown() {
	c.once.Do(func() {
		close(c.done)
		c.mu.Lock()
		defer c.mu.Unlock()
		for id, ch := range c.pending {
			ch <- rpcReply{err: errConnClosed}
			delete(c.pending, id)
		}
	})
}

// call 发送一个请求并等待其响应。
// ctx 取消时按 stdio 取消语义发送 notifications/cancelled(规范 §stdio Cancellation)。
func (c *rpcClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++ // MRTR 重试必须使用新的 JSON-RPC id(规范 §server/tools)
	id := c.nextID
	ch := make(chan rpcReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			c.takePending(id)
			return nil, fmt.Errorf("mcp: encode params: %w", err)
		}
		req.Params = p
	}
	frame, err := json.Marshal(req)
	if err != nil {
		c.takePending(id)
		return nil, fmt.Errorf("mcp: encode request: %w", err)
	}
	if err := c.tr.Send(frame); err != nil {
		c.takePending(id)
		return nil, fmt.Errorf("mcp: send: %w", err)
	}

	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, reply.err
		}
		return reply.result, nil
	case <-ctx.Done():
		c.takePending(id)
		c.notifyCancelled(id)
		return nil, ctx.Err()
	case <-c.done:
		return nil, errConnClosed
	}
}

func (c *rpcClient) notifyCancelled(id int64) {
	params, _ := json.Marshal(map[string]any{"requestId": id})
	frame, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/cancelled", Params: params})
	if err == nil {
		_ = c.tr.Send(frame)
	}
}

// notify 发送一个通知(无 id,不等待响应)。
func (c *rpcClient) notify(method string, params any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("mcp: encode params: %w", err)
		}
		req.Params = p
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("mcp: encode notification: %w", err)
	}
	if err := c.tr.Send(frame); err != nil {
		return fmt.Errorf("mcp: send: %w", err)
	}
	return nil
}

// Close 关闭底层传输并让所有在途调用失败。
func (c *rpcClient) Close() error {
	err := c.tr.Close()
	c.shutdown()
	return err
}
