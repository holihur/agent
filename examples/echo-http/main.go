// Command echo-http 是最小 MCP Streamable HTTP 服务器示例(现代 2026-07-28 协议,零第三方依赖)。
//
// 与 examples/echo(stdio)提供相同的两个工具:
//
//	echo(text string) → 原样返回文本
//	now()             → 当前时间(RFC3339)
//
// 区别在传输:HTTP POST 进、text/event-stream 出(每请求一个 SSE 事件),
// 通知回 202;并演示 Mcp-Session-Id 的指派。
//
// 供 agent 接入演示:
//
//	go run ./examples/echo-http &
//	go run ./cmd/agent -mcp "echo=http://127.0.0.1:8787/mcp" -q "用 echo 工具返回 ping"
//
// 仅实现 agent 客户端走过的现代路径,不是通用 MCP 服务器;legacy 握手未实现。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

const protocolVersion = "2026-07-28"

// sessionID 是本示例的固定会话标识:agent 会在后续请求中原样回显。
const sessionID = "echo-http-demo"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"` // nil = 通知
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	flag.Parse()

	http.HandleFunc("/mcp", handle)
	log.Printf("echo-http: listening on http://%s/mcp", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "MCP transport is POST-only", http.StatusMethodNotAllowed)
		return
	}
	var fr request
	if err := json.NewDecoder(req.Body).Decode(&fr); err != nil {
		http.Error(w, "bad JSON-RPC frame", http.StatusBadRequest)
		return
	}

	// 通知:无响应帧,202 空体(规范 §transports)。
	if fr.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := dispatch(&fr)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      fr.ID,
		"result":  result,
		"error":   rpcErr,
	})
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}

	// 应答走 text/event-stream:一个 SSE 事件承载本响应。
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Mcp-Session-Id", sessionID)
	fmt.Fprint(w, "event: message\ndata: ")
	w.Write(body)
	fmt.Fprint(w, "\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// dispatch 与 examples/echo 同一套工具逻辑(示例刻意保持独立,不 import 内部包)。
func dispatch(fr *request) (any, *rpcError) {
	switch fr.Method {
	case "server/discover":
		return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
	case "tools/list":
		return map[string]any{
			"resultType": "complete",
			"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "原样返回传入的 text",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{"type": "string", "description": "要回显的文本"},
						},
						"required": []string{"text"},
					},
				},
				{
					"name":        "now",
					"description": "返回服务器当前时间(RFC3339)",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
			"nextCursor": "",
			"ttlMs":      60000,
		}, nil
	case "tools/call":
		return callTool(fr)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + fr.Method}
	}
}

func callTool(fr *request) (any, *rpcError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(fr.Params, &params) != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}

	var text string
	switch params.Name {
	case "echo":
		var args struct {
			Text string `json:"text"`
		}
		if len(params.Arguments) > 0 && json.Unmarshal(params.Arguments, &args) != nil {
			return nil, &rpcError{Code: -32602, Message: `arguments must be {"text": string}`}
		}
		text = args.Text
	case "now":
		text = time.Now().Format(time.RFC3339)
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}
	}

	return map[string]any{
		"resultType": "complete",
		"content":    []map[string]any{{"type": "text", "text": text}},
		"isError":    false,
	}, nil
}
