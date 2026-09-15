// Package apisrv 把 agent 暴露为一个常驻的 HTTP 服务：面向浏览器的聊天 UI +
// JSON/SSE 聊天 API，供 gitdash 等宿主或人类直接使用。
//
// 与 MCP 服务器模式（-addr，工具 agent_run/agent_sessions，无 shell/fs）不同，
// apisrv 面向"编码 copilot"：默认开启内置 shell 与 read/write/edit 文件工具，
// 以进程 cwd（通常由 -C 指定为仓库工作区）为根；文本增量与工具调用经 SSE
// 双向流式回传。
//
// 协议（所有响应 JSON；/api/chat 为 text/event-stream）：
//
//	GET  /healthz                  → {"ok":true}
//	GET  /api/sessions             → {"sessions":["<name>", ...]}
//	POST /api/chat   {session,text} → SSE：delta / tool_start / tool_end / done / error
//	POST /api/cancel {session}      → {"canceled":bool}
//	GET  /                          → 内置聊天 UI
package apisrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/llm"
	"github.com/holihur/agent/internal/session"
	"github.com/holihur/agent/internal/tools"
)

// Options 是 API 服务器的装配配置。
type Options struct {
	Addr            string // 监听地址，如 127.0.0.1:8790
	System          string // system prompt
	APIKey          string // LLM 密钥（必填）
	BaseURL         string // 端点（必填；协议见 API）
	Model           string // 模型名（必填）
	API             string // anthropic(默认)|openai|responses；空 = anthropic
	AuthStyle       string // bearer | x-api-key | both（仅 anthropic）
	MaxTokens       int
	MaxTurns        int
	Temperature     *float64
	ReasoningEffort string
	SessionDir      string // 会话持久化目录；空 → .agent/sessions
}

// ProtocolVersion 标识 gitdash ↔ agent 的聊天协议版本（见 docs/copilot-api.md）。
const ProtocolVersion = "copilot-api/1"

// Event 是一条 SSE 下行事件。
type Event struct {
	Type    string `json:"type"` // delta | tool_start | tool_end | done | error | status
	Text    string `json:"text,omitempty"`
	Name    string `json:"name,omitempty"`
	Input   string `json:"input,omitempty"`
	Result  string `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Status  string `json:"status,omitempty"`
}

// ChatMessage 是回放给宿主的单条对话。
type ChatMessage struct {
	Role  string      `json:"role"` // user | assistant
	Text  string      `json:"text,omitempty"`
	Tools []ToolEvent `json:"tools,omitempty"`
}

// ToolEvent 是 ChatMessage 中的一次工具调用及其结果。
type ToolEvent struct {
	Name    string `json:"name"`
	Input   string `json:"input,omitempty"`
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// Server 持有按会话名惰性构造的 agent 池。
type Server struct {
	opts  Options
	store *session.FileStore
	mu    sync.Mutex
	sess  map[string]*sessionAgent

	// llmFactory 仅用于测试注入假 LLM；nil 时按 opts.API 构造真实协议客户端。
	llmFactory func() agent.LLM
}

// sessionAgent 包装一个常驻 agent 实例及其并发/流式状态。
type sessionAgent struct {
	name  string
	ag    *agent.Agent
	runMu sync.Mutex // 串行化同一会话的轮次

	stateMu sync.Mutex
	cancel  context.CancelFunc
	emit    func(Event)
}

func (sa *sessionAgent) send(ev Event) {
	sa.stateMu.Lock()
	emit := sa.emit
	sa.stateMu.Unlock()
	if emit != nil {
		emit(ev)
	}
}

func (sa *sessionAgent) setState(cancel context.CancelFunc, emit func(Event)) {
	sa.stateMu.Lock()
	sa.cancel, sa.emit = cancel, emit
	sa.stateMu.Unlock()
}

func (sa *sessionAgent) clearState() {
	sa.stateMu.Lock()
	sa.cancel, sa.emit = nil, nil
	sa.stateMu.Unlock()
}

func (sa *sessionAgent) stop() {
	sa.stateMu.Lock()
	cancel := sa.cancel
	sa.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Run 启动 HTTP 服务器并阻塞。
func Run(opts Options) error {
	s, err := newServer(opts)
	if err != nil {
		return err
	}
	log.Printf("apisrv: listening on http://%s (chat UI at /)", opts.Addr)
	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// newServer 校验配置并构造服务器（不监听）。
func newServer(opts Options) (*Server, error) {
	if opts.APIKey == "" || opts.BaseURL == "" || opts.Model == "" {
		return nil, errors.New("apisrv: APIKey, BaseURL and Model are required")
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:8790"
	}
	dir := opts.SessionDir
	if dir == "" {
		dir = ".agent/sessions"
	}
	return &Server{opts: opts, store: session.NewFileStore(dir), sess: map[string]*sessionAgent{}}, nil
}

// Handler 返回路由（供宿主/测试复用）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/messages", s.handleMessages)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

// get 返回（或惰性构造）指定会话的 agent。
func (s *Server) get(name string) (*sessionAgent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sa, ok := s.sess[name]; ok {
		return sa, nil
	}
	sa := &sessionAgent{name: name}
	ag, err := s.buildAgent(sa)
	if err != nil {
		return nil, err
	}
	if msgs, err := s.store.Load(context.Background(), name); err == nil {
		ag.Messages = msgs
	} else if !errors.Is(err, agent.ErrSessionNotFound) {
		return nil, err
	}
	sa.ag = ag
	s.sess[name] = sa
	return sa, nil
}

// buildAgent 构造一个开启 shell/fs 的 agent，并把流式与工具事件接到会话。
func (s *Server) buildAgent(sa *sessionAgent) (*agent.Agent, error) {
	registry := tools.New()
	local := tools.NewLocal()
	if err := tools.RegisterShell(local); err != nil {
		return nil, err
	}
	if err := tools.RegisterFS(local); err != nil {
		return nil, err
	}
	if err := registry.Register(local); err != nil {
		return nil, err
	}

	hooks := agent.NewHooks()
	hooks.OnBeforeTool(func(c agent.ToolCall) agent.Decision {
		sa.send(Event{Type: "tool_start", Name: c.Name, Input: string(c.Input)})
		return agent.Decision{}
	})
	hooks.OnAfterTool(func(o agent.ToolOutcome) {
		sa.send(Event{Type: "tool_end", Name: o.Name, Result: o.Text, IsError: o.IsError || o.Denied})
	})

	client := agent.LLM(nil)
	if s.llmFactory != nil {
		client = s.llmFactory()
	} else {
		// 与 CLI/嵌入式一致：按 API 选择协议适配器(anthropic|openai|responses)。
		c, err := llm.NewAdapter(s.opts.API, llm.Config{
			APIKey:          s.opts.APIKey,
			BaseURL:         s.opts.BaseURL,
			Model:           s.opts.Model,
			MaxTokens:       s.opts.MaxTokens,
			AuthStyle:       s.opts.AuthStyle,
			Temperature:     s.opts.Temperature,
			ReasoningEffort: s.opts.ReasoningEffort,
		})
		if err != nil {
			return nil, err
		}
		client = c
	}

	ag := &agent.Agent{
		LLM:      client,
		Registry: registry,
		System:   s.opts.System,
		Hooks:    hooks,
		MaxTurns: s.opts.MaxTurns,
	}
	ag.OnTextDelta = func(d agent.TextDelta) { sa.send(Event{Type: "delta", Text: d.Text}) }
	return ag, nil
}

// ---- HTTP handlers ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := len(s.sess)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "protocol": ProtocolVersion, "sessions": n})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	names, err := s.store.Names(context.Background())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	s.mu.Lock()
	for n := range s.sess {
		seen[n] = true
	}
	s.mu.Unlock()
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

type chatRequest struct {
	Session string `json:"session"`
	Text    string `json:"text"`
}

// handleMessages 返回某会话已持久化的对话历史（供宿主回放）。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("session"))
	if name == "" {
		name = "default"
	}
	msgs, err := s.store.Load(context.Background(), name)
	if err != nil && !errors.Is(err, agent.ErrSessionNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": projectMessages(msgs)})
}

// projectMessages 把 agent 领域消息投影为宿主可渲染的对话列表。
func projectMessages(msgs []agent.Message) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	type ref struct{ msg, tool int }
	refs := map[string]ref{}
	for _, m := range msgs {
		cm := ChatMessage{Role: string(m.Role)}
		for _, b := range m.Blocks {
			switch b.Type {
			case agent.BlockText:
				if b.Text == "" {
					continue
				}
				if cm.Text != "" {
					cm.Text += "\n"
				}
				cm.Text += b.Text
			case agent.BlockToolUse:
				cm.Tools = append(cm.Tools, ToolEvent{Name: b.Name, Input: string(b.Input)})
				refs[b.ID] = ref{msg: len(out), tool: len(cm.Tools) - 1}
			case agent.BlockToolResult:
				if r, ok := refs[b.ToolUseID]; ok && r.msg < len(out) {
					out[r.msg].Tools[r.tool].Result = b.Content
					out[r.msg].Tools[r.tool].IsError = b.IsError
				}
			}
		}
		if cm.Text != "" || len(cm.Tools) > 0 {
			out = append(out, cm)
		}
	}
	return out
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in chatRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad JSON body", http.StatusBadRequest)
		return
	}
	in.Session = strings.TrimSpace(in.Session)
	if in.Session == "" {
		in.Session = "default"
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" {
		http.Error(w, "empty text", http.StatusBadRequest)
		return
	}
	sa, err := s.get(in.Session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sa.runMu.Lock()
	defer sa.runMu.Unlock()

	ctx, cancel := context.WithCancel(r.Context())
	sa.setState(cancel, func(ev Event) { _ = writeSSE(w, flusher, ev) })
	defer sa.clearState()
	defer cancel()

	answer, runErr := sa.ag.Run(ctx, in.Text)
	if saveErr := s.store.Save(context.Background(), in.Session, sa.ag.Messages); saveErr != nil {
		log.Printf("apisrv: save session %s: %v", in.Session, saveErr)
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			sa.send(Event{Type: "error", Error: "canceled"})
			return
		}
		sa.send(Event{Type: "error", Error: runErr.Error()})
		return
	}
	sa.send(Event{Type: "done", Text: answer})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	in.Session = strings.TrimSpace(in.Session)
	if in.Session == "" {
		in.Session = "default"
	}
	s.mu.Lock()
	sa := s.sess[in.Session]
	s.mu.Unlock()
	if sa == nil {
		writeJSON(w, http.StatusOK, map[string]any{"canceled": false})
		return
	}
	sa.stop()
	writeJSON(w, http.StatusOK, map[string]any{"canceled": true})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, f http.Flusher, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	f.Flush()
	return nil
}
