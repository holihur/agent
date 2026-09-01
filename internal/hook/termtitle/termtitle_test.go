package termtitle

import (
	"context"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/tools"
)

func TestEnabled(t *testing.T) {
	cases := map[string]bool{
		"": true, "on": true, "auto": true,
		"off": false, "none": false,
	}
	for mode, want := range cases {
		if got := enabled(mode); got != want {
			t.Errorf("enabled(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"3+5 等于几", "3+5 等于几"},
		{"hello\nworld", "hello world"},      // 换行压成空格
		{"a  \t b\t\rc", "a b c"},            // 连续空白折叠
		{"\x1b]0;evil\x07x", "]0;evilx"},     // OSC 注入:ESC/BEL 丢弃
		{"\x1b[31mred\x1b[0m", "[31mred[0m"}, // ANSI 转义丢弃
		{"   ", ""},                          // 纯空白 → 空(不写标题)
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeTruncates(t *testing.T) {
	long := strings.Repeat("字", maxTitleLen+10)
	got := sanitize(long)
	runes := []rune(got)
	if len(runes) != maxTitleLen || !strings.HasSuffix(got, "…") {
		t.Errorf("truncated title length = %d, want %d with ellipsis suffix", len(runes), maxTitleLen)
	}
}

// captureTitle 替换 writeTitle 出口收集写入内容;返回恢复函数。
func captureTitle(t *testing.T) (*[]string, func()) {
	t.Helper()
	var got []string
	old := writeTitle
	writeTitle = func(title string) { got = append(got, title) }
	return &got, func() { writeTitle = old }
}

type stubLLM struct{ called bool }

func (s *stubLLM) Turn(context.Context, agent.TurnRequest) (agent.TurnResult, error) {
	s.called = true
	return agent.TurnResult{StopReason: "end_turn"}, nil
}

func TestRunStartWritesTitle(t *testing.T) {
	titles, restore := captureTitle(t)
	defer restore()

	h := agent.NewHooks()
	if err := installTermTitle(h, hook.Deps{}); err != nil {
		t.Fatalf("installTermTitle: %v", err)
	}
	ag := &agent.Agent{LLM: &stubLLM{}, Registry: tools.New(), Hooks: h}
	if _, err := ag.Run(context.Background(), "3+5 等于几"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"3+5 等于几"}; len(*titles) != 1 || (*titles)[0] != want[0] {
		t.Fatalf("titles = %q, want %q", *titles, want)
	}
}

func TestRunStartSkipsBlankTitle(t *testing.T) {
	titles, restore := captureTitle(t)
	defer restore()

	h := agent.NewHooks()
	if err := installTermTitle(h, hook.Deps{}); err != nil {
		t.Fatalf("installTermTitle: %v", err)
	}
	ag := &agent.Agent{LLM: &stubLLM{}, Registry: tools.New(), Hooks: h}
	if _, err := ag.Run(context.Background(), " \n\t "); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*titles) != 0 {
		t.Fatalf("blank input must not write title, got %q", *titles)
	}
}

func TestOffNoHook(t *testing.T) {
	old := *termTitle
	defer func() { *termTitle = old }()
	*termTitle = "off"

	titles, restore := captureTitle(t)
	defer restore()

	h := agent.NewHooks()
	if err := installTermTitle(h, hook.Deps{}); err != nil {
		t.Fatalf("installTermTitle: %v", err)
	}
	ag := &agent.Agent{LLM: &stubLLM{}, Registry: tools.New(), Hooks: h}
	if _, err := ag.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*titles) != 0 {
		t.Fatalf("term-title off must not write title, got %q", *titles)
	}
}
