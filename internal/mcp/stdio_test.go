package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
)

// TestStdioRoundTrip 起一个真实子进程(以本测试二进制充当 MCP 服务器),
// 验证 NDJSON 分帧与请求/响应往返。子进程经 MCP_STDIO_HELPER 门控进入服务器模式。
func TestStdioRoundTrip(t *testing.T) {
	if os.Getenv("MCP_STDIO_HELPER") == "1" {
		runHelperServer()
		os.Exit(0)
	}

	tr, err := dialStdio(context.Background(), StdioConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestStdioRoundTrip"},
		Env:     []string{"MCP_STDIO_HELPER=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newRPCClient(tr)

	res, err := c.call(context.Background(), "test/echo", map[string]any{"hello": "world"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Echo struct {
			Hello string `json:"hello"`
		} `json:"echo"`
	}
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("decode result %s: %v", res, err)
	}
	if got.Echo.Hello != "world" {
		t.Fatalf("echo = %+v", got)
	}

	// 第二次调用:验证连接可复用(非一次性)。
	if _, err := c.call(context.Background(), "test/echo", map[string]any{"n": 2}); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// runHelperServer 是子进程内的迷你 MCP 服务器:
// 读 stdin 的 NDJSON 请求,回显 params 作为 result(一行一帧)。
func runHelperServer() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrameSize)
	w := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		var req rpcRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		resp := rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  mustJSONRaw(map[string]any{"echo": json.RawMessage(req.Params)}),
		}
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		w.Write(b)
		w.WriteByte('\n')
		if err := w.Flush(); err != nil {
			return
		}
	}
}
