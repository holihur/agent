package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

// captureServer 记录请求体并回放最小合法响应,供断言 wire 字段。
func captureServer(t *testing.T, captured *map[string]any) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(captured)
		_, _ = w.Write([]byte(`{"role":"assistant","content":[]}`))
	}))
	t.Cleanup(ts.Close)
	c := New("k", ts.URL, "m", 100)
	return c
}

func TestTemperatureSentWhenSet(t *testing.T) {
	captured := map[string]any{}
	c := captureServer(t, &captured)
	temp := 0.2
	c.Temperature = &temp

	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, ok := captured["temperature"].(float64); !ok || got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2 on the wire", captured["temperature"])
	}
}

func TestTemperatureZeroIsSent(t *testing.T) {
	captured := map[string]any{}
	c := captureServer(t, &captured)
	temp := 0.0
	c.Temperature = &temp

	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, ok := captured["temperature"].(float64); !ok || got != 0 {
		t.Fatalf("temperature = %v, want explicit 0 (deterministic)", captured["temperature"])
	}
}

func TestTemperatureOmittedWhenNil(t *testing.T) {
	captured := map[string]any{}
	c := captureServer(t, &captured)

	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := captured["temperature"]; ok {
		t.Fatalf("temperature = %v, want field absent", captured["temperature"])
	}
}

func TestTemperatureSentOnStream(t *testing.T) {
	captured := map[string]any{}
	c := captureServer(t, &captured)
	temp := 0.7
	c.Temperature = &temp

	if _, err := c.TurnStream(context.Background(), agent.TurnRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := captured["temperature"].(float64); !ok || got != 0.7 {
		t.Fatalf("stream temperature = %v, want 0.7", captured["temperature"])
	}
}

func TestReasoningEffortSentWhenSet(t *testing.T) {
	captured := map[string]any{}
	c := captureServer(t, &captured)
	c.ReasoningEffort = "high"

	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, ok := captured["reasoning_effort"].(string); !ok || got != "high" {
		t.Fatalf("reasoning_effort = %v, want high", captured["reasoning_effort"])
	}
}

func TestReasoningEffortOmittedWhenEmpty(t *testing.T) {
	captured := map[string]any{}
	c := captureServer(t, &captured)

	if _, err := c.Turn(context.Background(), agent.TurnRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := captured["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort = %v, want field absent", captured["reasoning_effort"])
	}
}
