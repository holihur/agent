package streamdown

import (
	"strings"
	"testing"
)

func TestWcwidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
	}{
		{0, 0},
		{0x01, -1},
		{0x1B, -1},
		{0x08, -1},
		{'a', 1},
		{' ', 1},
		{0x4E00, 2}, // CJK ideograph
		{0x3000, 2}, // ideographic space
		{0x03A9, 1}, // Greek omega (East Asian Ambiguous → 1)
		{0x2026, 1}, // ellipsis (Ambiguous → 1)
		{0x2190, 1}, // arrow (Ambiguous → 1)
		{0x200B, 0}, // zero-width space
		{0x0301, 0}, // combining acute
		{0x1F600, 2},
	}
	for _, tc := range cases {
		if got := wcwidth(tc.r); got != tc.want {
			t.Errorf("wcwidth(%U) = %d, want %d", tc.r, got, tc.want)
		}
	}
}

func TestVisibleLength(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"你好", 4},
		{"\x1b[31mred\x1b[0m", 3},
		{"a\x01b", 1}, // control chars subtract
		{"中文 with spaces", 16},
	}
	for _, tc := range cases {
		if got := visibleLength(tc.in); got != tc.want {
			t.Errorf("visibleLength(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSplitText(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"hello world", []string{"hello", "world"}},
		{"a  b", []string{"a", "b"}},
		{"你好世界", []string{"你", "好", "世", "界"}},
		{"abc你def", []string{"abc你", "def"}},
		{"你abc", []string{"你", "abc"}},
		{"word:next", []string{"word:next"}},
	}
	for _, tc := range cases {
		got := splitText(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitText(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitText(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"\x1b[K", ""},
		{"plain", "plain"},
	}
	for _, tc := range cases {
		if got := stripANSI(tc.in); got != tc.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAnsiCollapse(t *testing.T) {
	fg1 := "\x1b[38;2;1;2;3m"
	fg2 := "\x1b[38;2;4;5;6m"
	b := "\x1b[1m"
	reset := "\x1b[0m"

	// A new fg drops the buffered fg, keeps other classes.
	got := ansiCollapse([]string{fg1, b}, []string{fg2})
	want := []string{b, fg2}
	if len(got) != len(want) {
		t.Fatalf("ansiCollapse = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ansiCollapse = %q, want %q", got, want)
		}
	}

	// A reset short-circuits: only the new codes survive.
	if got := ansiCollapse([]string{fg1, b}, []string{reset}); len(got) != 1 || got[0] != reset {
		t.Errorf("ansiCollapse reset = %q", got)
	}
}

func TestStripStyled(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  hello  ", "hello"},
		{"\nhello\n", "hello"},
		{"\x1b[31m参数:\x1b[0m", "\x1b[31m参数:\x1b[0m"},
		// Escapes interleaved with boundary whitespace are transparent but kept.
		{"\x1b[31m\n\x1b[32m参数:\x1b[0m\n", "\x1b[31m\x1b[32m参数:\x1b[0m"},
		{"\n\x1b[31m\n\x1b[32m", "\x1b[31m\x1b[32m"}, // whitespace-only with escapes → no visible content
	}
	for _, tc := range cases {
		got := stripStyled(tc.in)
		if got != tc.want {
			t.Errorf("stripStyled(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.TrimSpace(stripANSI(got)) == "" && strings.TrimSpace(stripANSI(tc.want)) != "" {
			t.Errorf("stripStyled(%q) dropped visible content: %q", tc.in, got)
		}
	}
}

func TestTextWrapBasics(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Width = 20
	r, err := New(&strings.Builder{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// "hello world" (11) fits in width 20 with margins; no wrap needed.
	lines := r.textWrap("hello world", -1, 0, "", "", false, false)
	if len(lines) != 1 || !strings.Contains(stripANSI(lines[0]), "hello world") {
		t.Errorf("unexpected wrap: %q", lines)
	}

	// Long ASCII wraps into multiple lines.
	lines = r.textWrap(strings.Repeat("word ", 20), -1, 0, "", "", false, false)
	if len(lines) < 2 {
		t.Errorf("expected wrapping, got %d lines", len(lines))
	}
	for _, l := range lines {
		if visibleLength(l) > 20 {
			t.Errorf("line overflows width 20: %q (%d)", stripANSI(l), visibleLength(l))
		}
	}
}

func TestCJKCount(t *testing.T) {
	if got := cjkCount("你好"); got != 2 {
		t.Errorf("cjkCount(你好) = %d", got)
	}
	if got := cjkCount("abc"); got != 0 {
		t.Errorf("cjkCount(abc) = %d", got)
	}
	if got := cjkCount("\x1b[31m你\x1b[0m"); got != 1 {
		t.Errorf("cjkCount with ANSI = %d", got)
	}
}
