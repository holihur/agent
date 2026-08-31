package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/holihur/agent/internal/tools"
)

// ---- 测试替身:处理器式传输(按 method 生成应答) ----

type handlerTransport struct {
	t       *testing.T
	mu      sync.Mutex
	frames  [][]byte
	methods []string
	replies chan []byte
	closed  chan struct{}
	once    sync.Once
	handle  func(method string, params map[string]any) (any, *rpcError)
}

func newHandlerTransport(t *testing.T, handle func(method string, params map[string]any) (any, *rpcError)) *handlerTransport {
	return &handlerTransport{
		t: t, handle: handle,
		replies: make(chan []byte, 64),
		closed:  make(chan struct{}),
	}
}

func (h *handlerTransport) Send(b []byte) error {
	var req struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		h.t.Errorf("bad frame: %v", err)
		return nil
	}
	h.mu.Lock()
	h.frames = append(h.frames, append([]byte(nil), b...))
	if req.Method != "" {
		h.methods = append(h.methods, req.Method)
	}
	h.mu.Unlock()

	if req.ID == nil {
		return nil // notification:无应答
	}
	var params map[string]any
	_ = json.Unmarshal(req.Params, &params)
	result, rpcErr := h.handle(req.Method, params)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = mustJSONRaw(result)
	}
	h.replies <- mustJSONRaw(resp)
	return nil
}

func (h *handlerTransport) Recv() ([]byte, error) {
	select {
	case b := <-h.replies:
		return b, nil
	case <-h.closed:
		return nil, io.EOF
	}
}

func (h *handlerTransport) Close() error {
	h.once.Do(func() { close(h.closed) })
	return nil
}

func (h *handlerTransport) countMethod(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.methods {
		if m == method {
			n++
		}
	}
	return n
}

// checkMeta 断言每个请求都携带必填 _meta(规范 §basic/index)。
func checkMeta(t *testing.T, params map[string]any) {
	t.Helper()
	meta, ok := params["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("params missing _meta: %v", params)
	}
	if meta[metaProtocolVersion] != protocolVersion {
		t.Fatalf("_meta.protocolVersion = %v", meta[metaProtocolVersion])
	}
	if _, ok := meta[metaClientCaps]; !ok {
		t.Fatalf("_meta missing clientCapabilities: %v", meta)
	}
}

// newTestProvider 构造走 handler 传输的 Provider(绕过真实子进程)。
func newTestProvider(t *testing.T, ht *handlerTransport, responder tools.Responder) *Provider {
	return &Provider{
		name:      "fs",
		dial:      func(context.Context) (Transport, error) { return ht, nil },
		responder: responder,
	}
}

// ---- 时代探测 ----

func TestProbeModernServer(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		checkMeta(t, params)
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/list":
			return map[string]any{
				"resultType": "complete",
				"tools": []map[string]any{{
					"name": "read_file", "description": "Read a file",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
						"path": map[string]any{"type": "string"},
					}},
				}},
				"nextCursor": "", "ttlMs": 60000,
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unexpected " + method}
	})
	p := newTestProvider(t, ht, nil)

	defs, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "read_file" {
		t.Fatalf("defs = %+v", defs)
	}
	// 第一个请求必须是 server/discover(规范:现代客户端也 RECOMMENDED 先探测)。
	if m := ht.methods[0]; m != "server/discover" {
		t.Fatalf("first method = %q", m)
	}
}

func TestLegacyFallbackHandshakeAndList(t *testing.T) {
	var initParams map[string]any
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "server/discover":
			return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found"}
		case "initialize":
			initParams = params
			return map[string]any{
				"protocolVersion": legacyProtocolVersion,
				"serverInfo":      map[string]any{"name": "legacy", "version": "1.0"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}, nil
		case "tools/list":
			if _, has := params["_meta"]; has {
				t.Errorf("legacy request must not carry _meta: %v", params)
			}
			return map[string]any{
				"tools": []map[string]any{{"name": "legacy_tool", "description": "d", "inputSchema": map[string]any{"type": "object"}}},
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, nil)

	defs, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "legacy_tool" {
		t.Fatalf("defs = %+v", defs)
	}
	if p.era != eraLegacy {
		t.Fatalf("era = %q", p.era)
	}
	if initParams == nil || initParams["protocolVersion"] != legacyProtocolVersion {
		t.Fatalf("initialize params = %v", initParams)
	}
	if n := ht.countMethod("notifications/initialized"); n != 1 {
		t.Fatalf("initialized notifications = %d, want 1", n)
	}
}

func TestLegacyCallToolMapsIsError(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "server/discover":
			return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found"}
		case "initialize":
			return map[string]any{"protocolVersion": legacyProtocolVersion}, nil
		case "tools/call":
			if _, has := params["_meta"]; has {
				t.Errorf("legacy request must not carry _meta")
			}
			// legacy 结果无 resultType 信封(缺省按 complete 处理)。
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": "boom"}},
				"isError": true,
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, nil)

	res, err := p.CallTool(context.Background(), "legacy_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Text != "boom" {
		t.Fatalf("result = %+v", res)
	}
}

func TestUnusableServerErrorsClearly(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		return nil, &rpcError{Code: codeInternalError, Message: "broken"}
	})
	p := newTestProvider(t, ht, nil)
	_, err := p.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "neither a modern") {
		t.Fatalf("err = %v, want clear neither-modern-nor-legacy error", err)
	}
}

func TestProbeUnsupportedVersionListsSupported(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		return nil, &rpcError{Code: codeUnsupportedProtocolVersion, Message: "Unsupported protocol version",
			Data: json.RawMessage(`{"supported":["2025-11-25"],"requested":"2026-07-28"}`)}
	})
	p := newTestProvider(t, ht, nil)
	_, err := p.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "2025-11-25") {
		t.Fatalf("err = %v, want supported list in error", err)
	}
}

// ---- 分页与缓存 ----

func TestListToolsPagination(t *testing.T) {
	page := 0
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		checkMeta(t, params)
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/list":
			page++
			cursor := ""
			name := fmt.Sprintf("tool_p%d", page)
			if page == 1 {
				cursor = "c2"
			}
			return map[string]any{
				"resultType": "complete",
				"tools":      []map[string]any{{"name": name, "description": "d", "inputSchema": map[string]any{"type": "object"}}},
				"nextCursor": cursor,
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, nil)

	defs, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].Name != "tool_p1" || defs[1].Name != "tool_p2" {
		t.Fatalf("defs = %+v", defs)
	}
	var second struct {
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	if err := json.Unmarshal(ht.frames[len(ht.frames)-1], &second); err != nil {
		t.Fatal(err)
	}
	if second.Params.Cursor != "c2" {
		t.Fatalf("second page cursor = %q", second.Params.Cursor)
	}
	if n := ht.countMethod("tools/list"); n != 2 {
		t.Fatalf("tools/list calls = %d", n)
	}
}

func TestListToolsTTLCache(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/list":
			return map[string]any{
				"resultType": "complete",
				"tools":      []map[string]any{{"name": "a", "description": "d", "inputSchema": map[string]any{"type": "object"}}},
				"ttlMs":      60000,
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, nil)
	if _, err := p.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := ht.countMethod("tools/list"); n != 1 {
		t.Fatalf("tools/list calls = %d, want 1 (ttl cache)", n)
	}
}

// ---- 结果映射 ----

func TestCallToolContentMapping(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		checkMeta(t, params)
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/call":
			return map[string]any{
				"resultType": "complete",
				"content": []map[string]any{
					{"type": "text", "text": "line one"},
					{"type": "image", "mimeType": "image/png"},
					{"type": "resource_link", "uri": "file:///x"},
				},
				"isError": false,
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, nil)

	res, err := p.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "line one\n[image: image/png omitted]\nresource_link: file:///x"
	if res.Text != want || res.IsError {
		t.Fatalf("result = %+v, want text %q", res, want)
	}
	// tools/call 的 arguments 必须是对象(空参发 {})。
	req := decodeFrame(t, ht.frames[len(ht.frames)-1])
	if string(req.Params) == "" || !strings.Contains(string(req.Params), `"arguments":{}`) {
		t.Fatalf("call params = %s", req.Params)
	}
}

func TestCallToolStructuredFallbackAndIsError(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/call":
			return map[string]any{
				"resultType":        "complete",
				"structuredContent": map[string]any{"a": 1},
				"isError":           true,
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, nil)

	res, err := p.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Text != `{"a":1}` {
		t.Fatalf("result = %+v", res)
	}
}

// ---- MRTR ----

type recordingResponder struct {
	got    tools.InputRequest
	answer string
	fail   bool
	callsN int
}

func (r *recordingResponder) Respond(_ context.Context, req tools.InputRequest) ([]tools.InputResponse, error) {
	r.callsN++
	r.got = req
	if r.fail {
		return nil, errors.New("user walked away")
	}
	return []tools.InputResponse{{Key: req.Prompts[0].Key, Content: map[string]any{"answer": r.answer}}}, nil
}

func TestMRTRRetryWithInputResponses(t *testing.T) {
	var sawRetry map[string]any
	var sawState string
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		checkMeta(t, params)
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/call":
			if _, ok := params["inputResponses"]; !ok {
				// 第一轮:追问。
				return map[string]any{
					"resultType": "input_required",
					"inputRequests": map[string]any{
						"confirm": map[string]any{
							"method": "elicitation/create",
							"params": map[string]any{
								"mode":    "form",
								"message": "Proceed?",
								"requestedSchema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"answer": map[string]any{"type": "string"},
									},
									"required": []string{"answer"},
								},
							},
						},
					},
					"requestState": "st1",
				}, nil
			}
			// 第二轮:校验重试载荷后放行。
			sawRetry, _ = params["inputResponses"].(map[string]any)
			sawState, _ = params["requestState"].(string)
			return map[string]any{
				"resultType": "complete",
				"content":    []map[string]any{{"type": "text", "text": "done"}},
			}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	resp := &recordingResponder{answer: "yes"}
	p := newTestProvider(t, ht, resp)

	res, err := p.CallTool(context.Background(), "dangerous", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.IsError {
		t.Fatalf("result = %+v", res)
	}
	// 追问投影:工具名、消息、必填标记。
	if resp.got.Tool != "dangerous" || len(resp.got.Prompts) != 1 {
		t.Fatalf("InputRequest = %+v", resp.got)
	}
	prompt := resp.got.Prompts[0]
	if prompt.Key != "confirm" || prompt.Message != "Proceed?" ||
		len(prompt.Fields) != 1 || !prompt.Fields[0].Required || prompt.Fields[0].Name != "answer" {
		t.Fatalf("InputPrompt = %+v", prompt)
	}
	// 重试载荷:action=accept + content + requestState;JSON-RPC id 已换新(由 rpcClient 保证)。
	ir, ok := sawRetry["confirm"].(map[string]any)
	if !ok || ir["action"] != "accept" {
		t.Fatalf("inputResponses = %v", sawRetry)
	}
	content, _ := ir["content"].(map[string]any)
	if content["answer"] != "yes" {
		t.Fatalf("content = %v", content)
	}
	if sawState != "st1" {
		t.Fatalf("requestState = %q", sawState)
	}
}

func TestMRTRResponderDeclineIsToolError(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/call":
			if _, ok := params["inputResponses"]; ok {
				return nil, &rpcError{Code: codeInternalError, Message: "should not retry after decline"}
			}
			return map[string]any{"resultType": "input_required", "inputRequests": map[string]any{
				"ask": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "?"}},
			}}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, &recordingResponder{fail: true})

	res, err := p.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "declined") {
		t.Fatalf("result = %+v", res)
	}
	if n := ht.countMethod("tools/call"); n != 1 {
		t.Fatalf("tools/call calls = %d, want 1 (no retry after decline)", n)
	}
}

func TestMRTRTooManyRounds(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/call":
			return map[string]any{"resultType": "input_required", "inputRequests": map[string]any{
				"ask": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "?"}},
			}}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, &recordingResponder{answer: "x"})

	res, err := p.CallTool(context.Background(), "tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "too many times") {
		t.Fatalf("result = %+v", res)
	}
	// 初始 + 3 轮追问 = 4 次调用。
	if n := ht.countMethod("tools/call"); n != maxMRTRRounds+1 {
		t.Fatalf("tools/call calls = %d, want %d", n, maxMRTRRounds+1)
	}
}

func TestCallToolUnknownResultType(t *testing.T) {
	ht := newHandlerTransport(t, func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "server/discover":
			return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
		case "tools/call":
			return map[string]any{"resultType": "from_the_future"}, nil
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: method}
	})
	p := newTestProvider(t, ht, nil)
	if _, err := p.CallTool(context.Background(), "tool", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected unknown resultType error")
	}
}
