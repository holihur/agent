package streamdown

import (
	"strings"
	"testing"
)

// renderString renders markdown with width 80 in plaintext mode and returns
// the stripped output, making feature assertions readable.
func renderString(t *testing.T, src string) string {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Width = 80
	cfg.Plaintext = true
	var buf strings.Builder
	r, err := New(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RenderString(src); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestHeadings(t *testing.T) {
	out := renderString(t, "# H1\n## H2\n### H3\n#### H4\n##### H5\n###### H6\n")
	for i, want := range []string{"H1", "H2", "H3", "H4", "H5", "H6"} {
		if !strings.Contains(out, want) {
			t.Errorf("heading %d missing %q in %q", i, want, out)
		}
	}
	// Centred H1/H2 are padded; # markers must not leak.
	if strings.Contains(out, "#") {
		t.Errorf("heading marker leaked: %q", out)
	}
}

func TestSetextHeadings(t *testing.T) {
	out := renderString(t, "Underlined\n==========\n\nAnother\n----------\n")
	if !strings.Contains(out, "Underlined") {
		t.Errorf("setext H1 missing: %q", out)
	}
	if !strings.Contains(out, "Another") {
		t.Errorf("setext H2 missing: %q", out)
	}
}

func TestInlineStyles(t *testing.T) {
	out := renderString(t, "**bold** *italic* ~~strike~~ `code` _underline_\n")
	for _, want := range []string{"bold", "italic", "strike", "code", "underline"} {
		if !strings.Contains(out, want) {
			t.Errorf("inline style missing %q in %q", want, out)
		}
	}
	// Markers themselves must not appear in the output.
	for _, mark := range []string{"**", "~~", "`"} {
		if strings.Contains(out, mark) {
			t.Errorf("marker %q leaked into output: %q", mark, out)
		}
	}
}

func TestLinks(t *testing.T) {
	// In plaintext mode the URL lives inside the stripped OSC 8 hyperlink, so
	// only the visible text remains; the raw ANSI output carries the URL.
	out := renderString(t, "[visit](https://example.com)\n")
	if !strings.Contains(out, "visit") {
		t.Errorf("link text missing: %q", out)
	}
	if strings.Contains(out, "[visit]") {
		t.Errorf("link marker leaked: %q", out)
	}

	cfg := DefaultConfig()
	cfg.Width = 80
	var buf strings.Builder
	r, err := New(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RenderString("[visit](https://example.com)\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "https://example.com") {
		t.Errorf("OSC 8 hyperlink URL missing: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\x1b]8;;https://example.com") {
		t.Errorf("OSC 8 open sequence missing: %q", buf.String())
	}
}

func TestCodeBlock(t *testing.T) {
	out := renderString(t, "```go\nfunc main() {\n\tprintln(\"hi\")\n}\n```\n")
	if !strings.Contains(out, "func main()") {
		t.Errorf("code block content missing: %q", out)
	}
	if !strings.Contains(out, "println") {
		t.Errorf("code block content missing: %q", out)
	}
	if strings.Contains(out, "```") {
		t.Errorf("fence leaked: %q", out)
	}
	// Code blocks are padded with the ▄/▀ half-block pads when PrettyPad is on.
	if !strings.Contains(out, "▄") || !strings.Contains(out, "▀") {
		t.Errorf("code pads missing: %q", out)
	}
}

func TestIndentedCodeBlock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Width = 80
	cfg.Plaintext = true
	trueVal := true
	cfg.CodeSpaces = &trueVal
	var buf strings.Builder
	r, err := New(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	src := "Intro text.\n\n    indented := true\n    println(indented)\n\nBack to normal.\n"
	if err := r.RenderString(src); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "indented := true") {
		t.Errorf("indented code block missing: %q", out)
	}
	if !strings.Contains(out, "Back to normal") {
		t.Errorf("text after indented code block missing: %q", out)
	}
}

func TestTables(t *testing.T) {
	out := renderString(t, "| A | B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |\n")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("table rendered %d lines: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "A") || !strings.Contains(lines[0], "B") {
		t.Errorf("table header wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], "1") || !strings.Contains(lines[2], "3") {
		t.Errorf("table body wrong: %q", out)
	}
}

func TestLists(t *testing.T) {
	out := renderString(t, "- one\n- two\n  1. sub-a\n  2. sub-b\n- three\n")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"one", "two", "sub-a", "sub-b", "three"} {
		if !strings.Contains(joined, want) {
			t.Errorf("list item %q missing: %q", want, joined)
		}
	}
	if !strings.Contains(joined, "•") {
		t.Errorf("bullet marker missing: %q", joined)
	}
}

func TestOrderedLists(t *testing.T) {
	out := renderString(t, "1. first\n2. second\n3. third\n")
	if !strings.Contains(out, "1 first") || !strings.Contains(out, "2 second") || !strings.Contains(out, "3 third") {
		t.Errorf("ordered list numbering wrong: %q", out)
	}
}

func TestBlockquote(t *testing.T) {
	out := renderString(t, "> quoted text\n> more\n")
	if !strings.Contains(out, "│") {
		t.Errorf("blockquote bar missing: %q", out)
	}
	if !strings.Contains(out, "quoted text") || !strings.Contains(out, "more") {
		t.Errorf("blockquote content missing: %q", out)
	}
}

func TestHorizontalRule(t *testing.T) {
	out := renderString(t, "above\n\n---\n\nbelow\n")
	if !strings.Contains(out, "─") {
		t.Errorf("horizontal rule missing: %q", out)
	}
}

func TestThinkBlock(t *testing.T) {
	out := renderString(t, "<think>\nreasoning here\n</think>\nanswer\n")
	if !strings.Contains(out, "reasoning here") {
		t.Errorf("think content missing: %q", out)
	}
	if !strings.Contains(out, "answer") {
		t.Errorf("content after think missing: %q", out)
	}
}

func TestPlaintextStripsANSI(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Plaintext = true
	var buf strings.Builder
	r, err := New(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RenderString("**bold** and `code`\n"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("plaintext mode leaked ANSI: %q", buf.String())
	}
}

func TestNoNewlineAtEOF(t *testing.T) {
	out := renderString(t, "final line without newline")
	if !strings.Contains(out, "final line without newline") {
		t.Errorf("unterminated final line lost: %q", out)
	}
}

func TestCRLFInput(t *testing.T) {
	out := renderString(t, "# Title\r\n\r\nbody text\r\n")
	if !strings.Contains(out, "Title") || !strings.Contains(out, "body text") {
		t.Errorf("CRLF input mangled: %q", out)
	}
}

func TestTabsConverted(t *testing.T) {
	out := renderString(t, "```\nif true {\n\tx := 1\n}\n```\n")
	if !strings.Contains(out, "x := 1") {
		t.Errorf("tab conversion broke code: %q", out)
	}
}

// TestMalformedInputNeverPanics feeds assorted degenerate inputs through the
// renderer, mirroring the "broken" fixtures the reference ships with.
func TestMalformedInputNeverPanics(t *testing.T) {
	inputs := []string{
		"",
		"```",
		"```\nunclosed",
		"*",
		"**bold",
		"| table |\n",
		"> \n\n\n\n",
		"\x00\x00\x00",
		"~~~~",
		"#",
		"1.\n2.\n",
		"<think>\n",
		"\\x1b[31m",
		strings.Repeat("a", 10000),
		strings.Repeat("中", 5000),
	}
	cfg := DefaultConfig()
	cfg.Width = 40
	for _, src := range inputs {
		var buf strings.Builder
		r, err := New(&buf, cfg)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("panic on input %q: %v", src, p)
				}
			}()
			_ = r.RenderString(src)
			r.Tidyup()
		}()
	}
}
