// Command echo-mcp 是最小 MCP stdio 服务器示例(现代 2026-07-28 协议,零第三方依赖)。
//
// 提供两个工具:
//
//	echo(text string) → 原样返回文本
//	now()             → 当前时间(RFC3339)
//
// 供 agent 接入演示:
//
//	go build -o /tmp/echo-mcp ./examples/echo
//	go run ./cmd/agent -mcp "echo=/tmp/echo-mcp" -q "用 echo 工具返回 ping"
//
// 仅实现 agent 客户端走过的现代路径(server/discover 探测 → tools/list → tools/call),
// 不是通用 MCP 服务器;legacy(2025-06-18)握手未实现。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// protocolVersion 与 agent 客户端(internal/mcp provider.go)保持一致。
const protocolVersion = "2026-07-28"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"` // nil = 通知
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	fmt.Fprintln(os.Stderr, "echo-mcp: listening on stdio")

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var req request
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		resp := handle(&req)
		if resp == nil {
			continue // 通知:不回帧
		}
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		out.Write(b)
		out.WriteByte('\n')
		if err := out.Flush(); err != nil {
			return
		}
	}
}

// handle 按方法分发;通知(ID 为 nil)返回 nil。
func handle(req *request) *response {
	if req.ID == nil {
		return nil // notifications/initialized 等:忽略
	}
	switch req.Method {
	case "server/discover":
		return ok(req.ID, map[string]any{"supportedVersions": []string{protocolVersion}})
	case "tools/list":
		return ok(req.ID, map[string]any{
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
		})
	case "tools/call":
		return callTool(req)
	default:
		return fail(req.ID, -32601, "Method not found: "+req.Method)
	}
}

func callTool(req *request) *response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(req.Params, &params) != nil {
		return fail(req.ID, -32602, "invalid params")
	}

	var text string
	switch params.Name {
	case "echo":
		var args struct {
			Text string `json:"text"`
		}
		if len(params.Arguments) > 0 && json.Unmarshal(params.Arguments, &args) != nil {
			return fail(req.ID, -32602, "arguments must be {\"text\": string}")
		}
		text = args.Text
	case "now":
		text = time.Now().Format(time.RFC3339)
	default:
		return fail(req.ID, -32602, "unknown tool: "+params.Name)
	}

	return ok(req.ID, map[string]any{
		"resultType": "complete",
		"content":    []map[string]any{{"type": "text", "text": text}},
		"isError":    false,
	})
}

func ok(id *int64, result any) *response {
	b, err := json.Marshal(result)
	if err != nil {
		return fail(id, -32603, "internal error")
	}
	return &response{JSONRPC: "2.0", ID: id, Result: b}
}

func fail(id *int64, code int, message string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}
