// Command agent-mcp 把嵌入式 agent 暴露为 MCP Streamable HTTP 服务器,
// 供其他 agent(经 internal/mcp 客户端或任意 MCP 宿主)远程调用。
//
// 运行:
//
//	LLM_API_KEY=... LLM_BASE_URL=... LLM_MODEL=... go run ./examples/agent-mcp
//
// 接入演示:
//
//	go run ./cmd/agent -mcp "agent=http://127.0.0.1:8788/mcp" -q "..."
//
// 暴露的工具:
//
//	agent_run(text, session?)  → 驱动 agent 完整循环,返回最终文本回答
//	agent_sessions()           → 列出全部会话名
//
// 每个 session 名对应一个常驻 Agent 实例(map + 互斥锁;Agent 非并发安全,
// 全部调用串行化)。无第三方依赖,仅实现 agent 客户端走过的现代协议路径。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	agent "github.com/holihur/agent"
	"github.com/holihur/agent/internal/mdns"
)

// port 从 "host:port" 地址取端口号;非法回落默认 8788。
func port(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 8788
	}
	n, err := net.LookupPort("tcp", p)
	if err != nil || n <= 0 {
		return 8788
	}
	return n
}

const protocolVersion = "2026-07-28"

const sessionID = "agent-mcp-demo"

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

// pool 是会话名 → Agent 实例的常驻池;全部方法经 mu 串行化。
type pool struct {
	mu      sync.Mutex
	agents  map[string]*agent.Agent
	timeout time.Duration
}

func newPool(timeout time.Duration) *pool {
	return &pool{agents: map[string]*agent.Agent{}, timeout: timeout}
}

// get 返回指定会话的 Agent,惰性构造(凭据缺省走 env)。
func (p *pool) get(name string) (*agent.Agent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.agents[name]; ok {
		return a, nil
	}
	a, err := agent.New()
	if err != nil {
		return nil, err
	}
	p.agents[name] = a
	return a, nil
}

func (p *pool) run(session, text string) (string, error) {
	a, err := p.get(session)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := context.Background()
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	return a.Run(ctx, text)
}

func (p *pool) names() ([]string, error) {
	a, err := p.get("")
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return a.SessionNames()
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8788", "listen address")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-call timeout")
	mdnsOn := flag.Bool("mdns", false, "announce this agent via mDNS (_agent-mcp._tcp) for discovery")
	name := flag.String("name", "agent-mcp", "mDNS instance name (requires -mdns)")
	flag.Parse()

	if *mdnsOn {
		// 广播播报的是对外可达地址;loopback 监听对外不可达,
		// 自动改绑全部接口(保留端口),否则远端 agent 发现后连不上。
		if host, p, err := net.SplitHostPort(*addr); err == nil &&
			(host == "127.0.0.1" || host == "localhost" || host == "::1") {
			*addr = net.JoinHostPort("", p)
			log.Printf("agent-mcp: -mdns with loopback addr; rebinding to %s for reachability", *addr)
		}
		stop, err := mdns.Announce(context.Background(), *name, port(*addr), map[string]string{
			"path": "/mcp",
		})
		if err != nil {
			log.Fatalf("agent-mcp: mdns announce: %v", err)
		}
		defer stop()
		log.Printf("agent-mcp: announcing %s.%s (http://%s)", *name, mdns.DefaultServiceType, *addr)
	}

	p := newPool(*timeout)
	http.HandleFunc("/mcp", func(w http.ResponseWriter, req *http.Request) {
		handle(p, w, req)
	})
	log.Printf("agent-mcp: listening on http://%s/mcp", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handle(p *pool, w http.ResponseWriter, req *http.Request) {
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

	result, rpcErr := dispatch(p, &fr)
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

func dispatch(p *pool, fr *request) (any, *rpcError) {
	switch fr.Method {
	case "server/discover":
		return map[string]any{"supportedVersions": []string{protocolVersion}}, nil
	case "tools/list":
		return map[string]any{
			"resultType": "complete",
			"tools": []map[string]any{
				{
					"name":        "agent_run",
					"description": "驱动 agent 执行一轮完整思考-行动-观察循环,返回最终文本回答。同一 session 连续调用即多轮会话。",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text":    map[string]any{"type": "string", "description": "用户输入"},
							"session": map[string]any{"type": "string", "description": "会话名;缺省为独立默认会话"},
						},
						"required": []string{"text"},
					},
				},
				{
					"name":        "agent_sessions",
					"description": "列出全部已持久化的会话名",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
			"nextCursor": "",
			"ttlMs":      60000,
		}, nil
	case "tools/call":
		return callTool(p, fr)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + fr.Method}
	}
}

func callTool(p *pool, fr *request) (any, *rpcError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(fr.Params, &params) != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}

	var text string
	switch params.Name {
	case "agent_run":
		var args struct {
			Text    string `json:"text"`
			Session string `json:"session"`
		}
		if len(params.Arguments) == 0 || json.Unmarshal(params.Arguments, &args) != nil {
			return nil, &rpcError{Code: -32602, Message: `arguments must be {"text": string, "session"?: string}`}
		}
		if args.Text == "" {
			return nil, &rpcError{Code: -32602, Message: "text is required"}
		}
		answer, err := p.run(args.Session, args.Text)
		if err != nil {
			return map[string]any{
				"resultType": "complete",
				"content":    []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
				"isError":    true,
			}, nil
		}
		text = answer
	case "agent_sessions":
		names, err := p.names()
		if err != nil {
			return map[string]any{
				"resultType": "complete",
				"content":    []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
				"isError":    true,
			}, nil
		}
		if len(names) == 0 {
			text = "(no saved sessions)"
		} else {
			b, _ := json.Marshal(names)
			text = string(b)
		}
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}
	}

	return map[string]any{
		"resultType": "complete",
		"content":    []map[string]any{{"type": "text", "text": text}},
		"isError":    false,
	}, nil
}
