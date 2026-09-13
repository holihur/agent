package apisrv

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

// fakeLLM 是固定回答的 LLM 桩（非流式）。
type fakeLLM struct{ answer string }

func (f fakeLLM) Turn(_ context.Context, _ agent.TurnRequest) (agent.TurnResult, error) {
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText(f.answer)}},
		StopReason: "end_turn",
	}, nil
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := newServer(Options{
		Addr: "127.0.0.1:0", System: "test", APIKey: "k", BaseURL: "http://example.invalid", Model: "m",
		SessionDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.llmFactory = func() agent.LLM { return fakeLLM{answer: "hello from fake"} }
	return s
}

func TestHealthAndIndex(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Fatalf("healthz = %v", out)
	}

	idx, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Body.Close()
	body, _ := io.ReadAll(idx.Body)
	if !strings.Contains(string(body), "/api/chat") {
		t.Fatalf("index page missing chat client")
	}
}

func TestChatSSEAndSessionList(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t).Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/chat", "application/json", strings.NewReader(`{"session":"s1","text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	data, _ := io.ReadAll(resp.Body)
	sse := string(data)
	if !strings.Contains(sse, `"type":"done"`) || !strings.Contains(sse, "hello from fake") {
		t.Fatalf("unexpected SSE: %s", sse)
	}

	// 会话应被持久化，并在 /api/sessions 中可见。
	sess, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Body.Close()
	var list struct {
		Sessions []string `json:"sessions"`
	}
	if err := json.NewDecoder(sess.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range list.Sessions {
		if n == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("session s1 not listed: %v", list.Sessions)
	}

	// 历史回放：应能看到用户与 assistant 两条。
	mresp, err := http.Get(ts.URL + "/api/messages?session=s1")
	if err != nil {
		t.Fatal(err)
	}
	defer mresp.Body.Close()
	var hist struct {
		Messages []ChatMessage `json:"messages"`
	}
	if err := json.NewDecoder(mresp.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, m := range hist.Messages {
		roles[m.Role] = true
	}
	if !roles["user"] || !roles["assistant"] {
		t.Fatalf("history missing roles: %+v", hist.Messages)
	}
}

func TestChatEmptyText(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t).Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/chat", "application/json", strings.NewReader(`{"session":"s1","text":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
