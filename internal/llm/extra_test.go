package llm

import (
	"testing"
)

func TestNewWithDifferentMaxTokens(t *testing.T) {
	c := New("key", "http://a", "model", 0)
	if c.MaxTokens != 1024 {
		t.Fatalf("got %d", c.MaxTokens)
	}
	c2 := New("key", "http://a", "model", 2048)
	if c2.MaxTokens != 2048 {
		t.Fatalf("got %d", c2.MaxTokens)
	}
	c3 := New("key", "http://a", "model", -5)
	if c3.MaxTokens != 1024 {
		t.Fatalf("got %d", c3.MaxTokens)
	}
}

func TestTruncateJSON(t *testing.T) {
	s := "short"
	if got := truncateJSON(s); got != s {
		t.Fatalf("got %q", got)
	}
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	got := truncateJSON(string(long))
	if len([]rune(got)) != 121 {
		t.Fatalf("len %d", len([]rune(got)))
	}
	if !contains(got, "…") {
		t.Fatal("should contain …")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
