package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPConfig 描述一个 Streamable HTTP 远程 MCP 服务器(规范 §mcp.json/remote)。
type HTTPConfig struct {
	URL     string
	Headers map[string]string // 附加请求头(如 Authorization)
}

// httpStreamTimeout 是单个 POST 的上限(含 SSE 流读取),防远端挂死拖垮调用方。
const httpStreamTimeout = 60 * time.Second

// dialHTTP 建立 Streamable HTTP 传输(规范 §transports:POST JSON-RPC,
// 响应为 application/json 单帧或 text/event-stream 流;通知回 202 无帧)。
func dialHTTP(_ context.Context, cfg HTTPConfig) (Transport, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: http url is empty")
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("mcp: http url must start with http:// or https://, got %q", cfg.URL)
	}
	base, cancel := context.WithCancel(context.Background())
	return &httpTransport{
		cfg:    cfg,
		base:   base,
		cancel: cancel,
		client: http.Client{},
		frames: make(chan []byte, 16),
		closed: make(chan struct{}),
	}, nil
}

// httpTransport 实现 Transport:每个 Send 是一次 POST,响应帧进入待读队列。
// 与 stdio 不同,HTTP 天然一问一答;读循环侧的 Recv 只是排队帧的消费者。
type httpTransport struct {
	cfg    HTTPConfig
	base   context.Context // Close 时取消 → 在途 POST 中断
	cancel context.CancelFunc
	client http.Client

	mu        sync.Mutex
	session   string // Mcp-Session-Id:服务器指派后必须回显(规范 §transports)
	frames    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func (t *httpTransport) Send(b []byte) error {
	select {
	case <-t.closed:
		return errClosed
	default:
	}

	ctx, cancel := context.WithTimeout(t.base, httpStreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("mcp: http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sid := t.session
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: http post: %w", err)
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.mu.Lock()
		t.session = s
		t.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusAccepted: // 通知:无响应帧
		_ = resp.Body.Close()
		return nil
	case strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"):
		return t.consumeSSE(b, resp)
	default:
		return t.consumeJSON(resp)
	}
}

// consumeJSON 读取单帧 application/json 响应;非 JSON-RPC 形状的错误体转明确错误。
func (t *httpTransport) consumeJSON(resp *http.Response) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameSize))
	if err != nil {
		return fmt.Errorf("mcp: http read: %w", err)
	}
	var msg struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if json.Unmarshal(body, &msg) == nil && (msg.ID != nil || msg.Result != nil || msg.Error != nil) {
		select {
		case t.frames <- body:
			return nil
		case <-t.closed:
			return errClosed
		}
	}
	return fmt.Errorf("mcp: http status %d: unexpected body: %.200s", resp.StatusCode, body)
}

// consumeSSE 读取 text/event-stream:逐事件解析 data 载荷,
// 丢弃通知与服务器主动请求,直到出现与本请求 id 匹配的响应帧(规范 §transports)。
func (t *httpTransport) consumeSSE(sent []byte, resp *http.Response) error {
	defer resp.Body.Close()

	var sentReq rpcRequest
	_ = json.Unmarshal(sent, &sentReq) // 发出去的帧必然是我们编的,解析失败等价于 id=nil(通知)

	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxFrameSize))
	sc.Buffer(make([]byte, 0, 64*1024), maxFrameSize)
	var data []string
	// deliver 处理一个完整 SSE 事件;命中目标响应时入队并返回 true。
	deliver := func() bool {
		if len(data) == 0 {
			return false
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var msg struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(payload), &msg) != nil {
			return false // 形状不符(如 ping/注释载荷):丢弃
		}
		if msg.Method != "" || msg.ID == nil || sentReq.ID == nil || *msg.ID != *sentReq.ID {
			return false // 通知或他帧:丢弃
		}
		select {
		case t.frames <- []byte(payload):
			return true
		case <-t.closed:
			return true
		}
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"): // SSE 注释/心跳
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if deliver() {
				return nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcp: http stream: %w", err)
	}
	if deliver() {
		return nil
	}
	if sentReq.ID == nil {
		return nil // 通知流:读完即成功
	}
	return fmt.Errorf("mcp: http stream ended without response")
}

// Recv 弹出一个已收到的响应帧;Close 后返回 errClosed。
func (t *httpTransport) Recv() ([]byte, error) {
	for {
		select {
		case b := <-t.frames:
			return b, nil
		default:
		}
		select {
		case b := <-t.frames:
			return b, nil
		case <-t.closed:
			select { // closed 与最后一帧可能同时就绪:再排空一次
			case b := <-t.frames:
				return b, nil
			default:
			}
			return nil, errClosed
		}
	}
}

// Close 取消在途请求并标记通道关闭。
func (t *httpTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.cancel()
	})
	return nil
}
