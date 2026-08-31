package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	core "github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/llm"
	"github.com/holihur/agent/internal/session"
)

// stubLLM 记录收到的 TurnRequest,返回预置结果;经包内字段注入 inner。
type stubLLM struct {
	got  core.TurnRequest
	resp core.TurnResult
}

func (s *stubLLM) Turn(_ context.Context, r core.TurnRequest) (core.TurnResult, error) {
	s.got = r
	return s.resp, nil
}

// envCreds 在测试期设置全套凭据 env,并在结束时还原。
func envCreds(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_BASE_URL", "http://localhost:1")
	t.Setenv("LLM_MODEL", "test-model")
}

func TestNewEnvDefaults(t *testing.T) {
	envCreds(t)
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	client, ok := a.inner.LLM.(*llm.Client)
	if !ok {
		t.Fatalf("inner LLM is %T, want *llm.Client", a.inner.LLM)
	}
	if client.APIKey != "test-key" || client.BaseURL != "http://localhost:1" || client.Model != "test-model" {
		t.Fatalf("client = %+v, want env values", client)
	}
}

func TestNewMissingCredentials(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_APIKEY", "")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_MODEL", "")
	if _, err := New(); err == nil || !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Fatalf("err = %v, want missing LLM_API_KEY", err)
	}
	t.Setenv("LLM_API_KEY", "k")
	if _, err := New(); err == nil || !strings.Contains(err.Error(), "LLM_BASE_URL") {
		t.Fatalf("err = %v, want missing LLM_BASE_URL", err)
	}
	t.Setenv("LLM_BASE_URL", "http://localhost:1")
	if _, err := New(); err == nil || !strings.Contains(err.Error(), "LLM_MODEL") {
		t.Fatalf("err = %v, want missing LLM_MODEL", err)
	}
}

func TestNewConfigOverridesEnv(t *testing.T) {
	envCreds(t)
	t.Setenv("LLM_MODEL", "env-model")
	a, err := New(Config{Model: "cfg-model", MaxTokens: 64, MaxTurns: 7})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	if got := a.inner.System; got != "" {
		t.Fatalf("System = %q, want empty", got)
	}
	if a.inner.MaxTurns != 7 {
		t.Fatalf("MaxTurns = %d, want 7", a.inner.MaxTurns)
	}
	if _, err := New(Config{}, Config{}); err == nil {
		t.Fatal("two Configs must be rejected")
	}
}

func TestToolRegisterAndDuplicate(t *testing.T) {
	envCreds(t)
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	schema := map[string]any{"type": "object"}
	if err := a.Tool("now", "returns time", schema, func(context.Context, json.RawMessage) (string, error) { return "t", nil }); err != nil {
		t.Fatalf("Tool: %v", err)
	}
	if err := a.Tool("now", "dup", schema, func(context.Context, json.RawMessage) (string, error) { return "", nil }); err == nil {
		t.Fatal("duplicate tool must fail")
	}
	names, err := a.ToolNames(context.Background())
	if err != nil {
		t.Fatalf("ToolNames: %v", err)
	}
	if len(names) != 1 || names[0] != "now" {
		t.Fatalf("names = %v, want [now]", names)
	}
}

func TestShellOptIn(t *testing.T) {
	envCreds(t)
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	names, _ := a.ToolNames(context.Background())
	for _, n := range names {
		if n == "shell" {
			t.Fatal("shell must be opt-in for embedded use")
		}
	}
	if err := a.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if err := a.Shell(); err == nil {
		t.Fatal("double Shell must fail")
	}
	names, _ = a.ToolNames(context.Background())
	found := false
	for _, n := range names {
		if n == "shell" {
			found = true
		}
	}
	if !found {
		t.Fatal("shell missing after Shell()")
	}
}

func TestMCPFailFast(t *testing.T) {
	envCreds(t)
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	if err := a.MCP(MCPSpec{}); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("err = %v, want missing name", err)
	}
	if err := a.MCP(MCPSpec{Name: "x"}); err == nil || !strings.Contains(err.Error(), "URL or Command") {
		t.Fatalf("err = %v, want missing URL/Command", err)
	}
	err = a.MCP(MCPSpec{Name: "dead", Command: []string{"/nonexistent/agent-test-binary"}})
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("err = %v, want preflight failure", err)
	}
	names, _ := a.ToolNames(context.Background())
	if len(names) != 0 {
		t.Fatalf("failed MCP must not register tools, got %v", names)
	}
}

func TestRunRoutesThroughRegistry(t *testing.T) {
	envCreds(t)
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	if err := a.Tool("now", "returns time", map[string]any{"type": "object"},
		func(context.Context, json.RawMessage) (string, error) { return "12:00", nil }); err != nil {
		t.Fatalf("Tool: %v", err)
	}
	stub := &stubLLM{resp: core.TurnResult{
		Assistant:  core.Message{Role: core.RoleAssistant, Blocks: []core.Block{core.NewText("ok")}},
		StopReason: "end_turn",
	}}
	a.inner.LLM = stub
	answer, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "ok" {
		t.Fatalf("answer = %q, want ok", answer)
	}
	if len(stub.got.Tools) != 1 || stub.got.Tools[0].Name != "now" {
		t.Fatalf("tools = %+v, want [now]", stub.got.Tools)
	}
}

func TestSessionDefaultStore(t *testing.T) {
	envCreds(t)
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	if _, ok := a.sessions.(*session.FileStore); !ok {
		t.Fatalf("default store = %T, want *session.FileStore", a.sessions)
	}
}

func TestSessionPersistenceRoundTrip(t *testing.T) {
	envCreds(t)
	a, err := New(Config{Sessions: session.NewFileStore(t.TempDir())})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	a.inner.LLM = &stubLLM{resp: core.TurnResult{
		Assistant:  core.Message{Role: core.RoleAssistant, Blocks: []core.Block{core.NewText("ok")}},
		StopReason: "end_turn",
	}}
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(a.inner.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(a.inner.Messages))
	}

	if err := a.SaveSession("work"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	a.NewSession()
	if len(a.inner.Messages) != 0 {
		t.Fatal("NewSession must clear history")
	}
	if err := a.LoadSession("work"); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(a.inner.Messages) != 2 {
		t.Fatalf("restored messages = %d, want 2", len(a.inner.Messages))
	}

	names, err := a.SessionNames()
	if err != nil {
		t.Fatalf("SessionNames: %v", err)
	}
	if len(names) != 1 || names[0] != "work" {
		t.Fatalf("names = %v, want [work]", names)
	}
	if err := a.DeleteSession("work"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := a.LoadSession("work"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("load deleted err = %v, want ErrSessionNotFound", err)
	}
}

type memStore struct{ data map[string][]core.Message }

func (m *memStore) Save(_ context.Context, name string, msgs []core.Message) error {
	m.data[name] = msgs
	return nil
}

func (m *memStore) Load(_ context.Context, name string) ([]core.Message, error) {
	msgs, ok := m.data[name]
	if !ok {
		return nil, core.ErrSessionNotFound
	}
	return msgs, nil
}

func (m *memStore) Names(context.Context) ([]string, error) {
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out, nil
}

func (m *memStore) Delete(_ context.Context, name string) error {
	delete(m.data, name)
	return nil
}

func TestSessionCustomStore(t *testing.T) {
	envCreds(t)
	store := &memStore{data: map[string][]core.Message{}}
	a, err := New(Config{Sessions: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	a.inner.Messages = []core.Message{{Role: core.RoleUser, Blocks: []core.Block{core.NewText("hi")}}}
	if err := a.SaveSession("mem"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if len(store.data["mem"]) != 1 {
		t.Fatal("custom store must receive the history")
	}
}
