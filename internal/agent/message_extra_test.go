package agent

import "testing"

func TestTextContentFallbackToThinking(t *testing.T) {
	m := Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockThinking, Text: "thinking text"},
	}}
	if got := m.TextContent(); got != "thinking text" {
		t.Fatalf("got %q", got)
	}
	m2 := Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockText, Text: "real text"},
		{Type: BlockThinking, Text: "thinking"},
	}}
	if got := m2.TextContent(); got != "real text" {
		t.Fatalf("got %q", got)
	}
	m3 := Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockThinking, Text: "  "},
	}}
	if got := m3.TextContent(); got != "" {
		t.Fatalf("empty thinking should be empty, got %q", got)
	}
	m4 := Message{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockText, Text: ""},
		{Type: BlockThinking, Text: "fallback"},
	}}
	// Text is empty string (but not whitespace trimmed? our code trims whitespace for text but still checks if s != "")
	// For empty string, it will still be considered as textParts with ""? Let's check logic
	// Our code: if s := strings.TrimSpace(b.Text); s != "" { append } else if b.Text != "" { append }
	// So empty string will not be appended, so it will fallback to thinking
	if got := m4.TextContent(); got != "fallback" {
		t.Fatalf("got %q", got)
	}
}

func TestTextContentEmpty(t *testing.T) {
	m := Message{Role: RoleAssistant, Blocks: []Block{}}
	if got := m.TextContent(); got != "" {
		t.Fatalf("got %q", got)
	}
}
