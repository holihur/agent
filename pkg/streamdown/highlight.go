package streamdown

import (
	"bytes"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// highlighter wraps a chroma lexer, style and true-colour formatter. It is the
// Go counterpart of the reference implementation's pygments pipeline.
type highlighter struct {
	lexer     chroma.Lexer
	formatter chroma.Formatter
	style     *chroma.Style
}

// newHighlighter builds a highlighter for the given language using the named
// chroma style. Unknown languages fall back to the generic lexer, matching the
// reference implementation's Bash fallback.
func newHighlighter(language, styleName string) *highlighter {
	lexer := lexers.Get(language)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	style := styles.Get(styleName)
	if style == nil {
		style = styles.Fallback
	}
	return &highlighter{
		lexer:     lexer,
		formatter: formatters.TTY16m,
		style:     style,
	}
}

// Highlight returns the ANSI-styled rendering of code. On any lexer/formatter
// failure it returns the plain text so rendering never breaks.
//
// The reference implementation's formatter (pygments) always terminates its
// output with a newline; chroma only echoes the input's own newlines. The
// trailing newline is a text token that the incremental tail-finding relies
// on, so it is appended here to keep the two in lockstep.
func (h *highlighter) Highlight(code string) string {
	it, err := h.lexer.Tokenise(nil, code)
	if err != nil {
		return code
	}
	var buf bytes.Buffer
	if err := h.formatter.Format(&buf, h.style, it); err != nil {
		return code
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}
