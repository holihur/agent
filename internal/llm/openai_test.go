package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

func TestParseOpenAIError(t *testing.T) {
	apiErr := parseOpenAIError([]byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`))
	if apiErr == nil || apiErr.Type != "invalid_request_error" || apiErr.Message != "bad" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if parseOpenAIError([]byte(`{"foo":1}`)) != nil {
		t.Fatal("want nil for non-error body")
	}
	if parseOpenAIError([]byte(`{oops`)) != nil {
		t.Fatal("want nil for invalid JSON")
	}
}

func TestOpenAIPostAuthAndDefaultClient(t *testing.T) {
	var gotAuth, gotCT string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotCT = r.Header.Get("content-type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	resp, err := openAIPost(context.Background(), ts.Client(), "k", ts.URL+"/v1/chat/completions", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer k" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}

	// nil client → 走 http.DefaultClient(用活服务器覆盖该分支)。
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts2.Close()
	resp2, err := openAIPost(context.Background(), nil, "", ts2.URL, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
}

func TestOpenAIPostTransportError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()
	if _, err := openAIPost(context.Background(), nil, "k", url, []byte(`{}`)); err == nil {
		t.Fatal("want transport error")
	}
}

func TestOpenAIErrorFromResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"m","type":"t"}}`))
	}))
	defer ts.Close()
	resp, _ := http.Get(ts.URL)
	apiErr := openAIErrorFromResponse(resp)
	if apiErr.Type != "t" || apiErr.Message != "m" || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("apiErr = %+v", apiErr)
	}

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`melted`))
	}))
	defer ts2.Close()
	resp2, _ := http.Get(ts2.URL)
	apiErr2 := openAIErrorFromResponse(resp2)
	if apiErr2.Type != "http_error" || apiErr2.Message != "melted" {
		t.Fatalf("apiErr2 = %+v", apiErr2)
	}
}

func TestChatArgumentsString(t *testing.T) {
	if got := chatArgumentsString(`{"a":1}`); got != `{"a":1}` {
		t.Fatalf("valid got %q", got)
	}
	if got := chatArgumentsString(""); got != "{}" {
		t.Fatalf("empty got %q", got)
	}
	if got := chatArgumentsString(`{"a":`); got != "{}" {
		t.Fatalf("corrupt got %q", got)
	}
}

func TestNewAdapterProtocols(t *testing.T) {
	cases := []struct {
		api                         string
		isAnthropic, isChat, isResp bool
	}{
		{"", true, false, false},
		{"anthropic", true, false, false},
		{"openai", false, true, false},
		{"chat", false, true, false},
		{"responses", false, false, true},
		{"openai-responses", false, false, true},
	}
	for _, tc := range cases {
		got, err := NewAdapter(tc.api, Config{APIKey: "k", BaseURL: "u", Model: "m"})
		if err != nil {
			t.Fatalf("%q: %v", tc.api, err)
		}
		switch {
		case tc.isAnthropic:
			if _, ok := got.(*Client); !ok {
				t.Errorf("%q: got %T, want *Client", tc.api, got)
			}
		case tc.isChat:
			if _, ok := got.(*ChatClient); !ok {
				t.Errorf("%q: got %T, want *ChatClient", tc.api, got)
			}
		case tc.isResp:
			if _, ok := got.(*ResponsesClient); !ok {
				t.Errorf("%q: got %T, want *ResponsesClient", tc.api, got)
			}
		}
	}
	if _, err := NewAdapter("gpt", Config{}); err == nil {
		t.Fatal("want error for unknown api")
	}
}

func TestNewAdapterSetsConfig(t *testing.T) {
	temp := 0.2
	got, err := NewAdapter("anthropic", Config{
		APIKey: "k", BaseURL: "u", Model: "m", MaxTokens: 5,
		AuthStyle: "x-api-key", Temperature: &temp, ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := got.(*Client)
	if c.AuthStyle != "x-api-key" || c.Temperature == nil || *c.Temperature != 0.2 ||
		c.ReasoningEffort != "high" || c.MaxTokens != 5 || c.Model != "m" {
		t.Fatalf("anthropic config = %+v", c)
	}
	if _, ok := got.(agent.LLM); !ok {
		t.Fatalf("got %T does not implement agent.LLM", got)
	}
}
