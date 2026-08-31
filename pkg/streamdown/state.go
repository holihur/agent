package streamdown

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Code identifies parser modes (mirrors the Python sdlib.Code class).
type Code int

const (
	CodeNone     Code = iota
	CodeSpaces        // 4-space indented code block
	CodeBacktick      // fenced (``` or <pre>) code block
	CodeFlush         // streaming prompt flush marker
)

// Table phases.
const (
	tableOff = iota
	tableHead
	tableBody
)

// EmitFlag controls the look-ahead buffer of the emit loop.
type EmitFlag int

const (
	EmitNone  EmitFlag = iota
	EmitH1             // setext "---" under a line → the buffered line becomes H1
	EmitH2             // setext "***/___" under a line → the buffered line becomes H2
	EmitFlush          // streaming prompt line: flush buffered output now
)

// ListItem is one entry of the list-indent stack.
type ListItem struct {
	Indent int
	Type   string // "bullet" or "number"
}

// ParseState holds all mutable state of the streaming parser. It is recreated
// for every Render call.
type ParseState struct {
	Style *Style

	buffer        []byte
	currentLine   string
	lastLineEmpty bool
	hasNewline    bool
	maybePrompt   bool
	emitFlag      EmitFlag

	// width bookkeeping; width = content width (WidthFull minus margins).
	widthArg  int
	widthFull int
	width     int
	widthWrap bool

	firstIndent *int
	bg          string // active background (code blocks/tables)

	// code blocks
	codeBuffer    string // incrementally highlighted raw code
	codeBufferRaw string // all raw code of the current block
	codeLanguage  string
	codeFirstLine bool
	codeIndent    int
	codeLine      string // accumulator until a newline arrives

	// lists
	orderedListNumbers []int
	listItemStack      []ListItem
	listIndentText     int
	inList             bool

	// inline / block state
	inCode      Code
	inlineCode  string
	inBold      bool
	inItalic    bool
	inTable     int
	inUnderline bool
	inStrikeout bool
	blockDepth  int
	blockType   string

	exit int
}

func newParseState(st *Style) *ParseState {
	bg := ANSIBGReset
	if st.PlainBackground {
		bg = "" // plain 模式不设背景,避免行尾/表格填充里出现 49m 复位噪音
	}
	return &ParseState{
		Style:         st,
		bg:            bg,
		lastLineEmpty: true,
	}
}

// bgReset 返回当前模式的背景复位码:plain 模式无背景,直接返回空串。
func bgReset(s *ParseState) string {
	if s.Style.PlainBackground {
		return ""
	}
	return ANSIBGReset
}

// currentNone reports whether no inline/block formatting is active (used for
// streaming prompt detection).
func (s *ParseState) currentNone() bool {
	return s.inlineCode == "" && s.inCode == CodeNone &&
		!s.inBold && !s.inItalic && !s.inUnderline && !s.inStrikeout
}

// resetInline clears all inline formatting flags after a chunk is emitted.
func (s *ParseState) resetInline() {
	s.inlineCode, s.inBold, s.inItalic = "", false, false
	s.inUnderline, s.inStrikeout = false, false
}

// spaceLeft returns the left prefix for the current context: margins,
// blockquote bars and (optionally) list indentation. It is empty while a line
// is being streamed (currentLine non-empty).
func (s *ParseState) spaceLeft(listwidth bool) string {
	pre := ""
	if listwidth {
		pre = strings.Repeat(" ", len(s.listItemStack)*s.Style.ListIndent)
	}
	if len(s.currentLine) == 0 {
		return s.Style.MarginSpaces + strings.Repeat(s.Style.Blockquote, s.blockDepth) + pre
	}
	return ""
}

// currentWidth is the usable width for the current line.
func (s *ParseState) currentWidth(listwidth bool) int {
	return s.width - utf8.RuneCountInString(stripANSI(s.spaceLeft(listwidth))) + s.Style.Margin
}

// fullWidth is the total width including the (optional) offset.
func (s *ParseState) fullWidth(offset int) int {
	if s.Style.PrettyBroken {
		return offset + s.currentWidth(true)
	}
	return offset + s.widthFull
}

// --- text helpers -----------------------------------------------------------

var (
	ansiEscapeRe = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[a-zA-Z]|\][0-9]*;;.*?\\|\\\\)`)
	sgrEscapeRe  = regexp.MustCompile(`\x1b\[[0-9;]*[mK]`)
)

// stripANSI removes all ANSI escape sequences (SGR, OSC, CSI) from s.
func stripANSI(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}

// extractANSICodes returns all SGR escape sequences present in s.
func extractANSICodes(s string) []string {
	return sgrEscapeRe.FindAllString(s, -1)
}

// visibleLength is the display width of s after stripping ANSI escapes,
// measured with the same wcwidth rules as the reference implementation
// (wide/CJK runes are 2 columns, combining/zero-width runes 0, control
// characters -1).
func visibleLength(s string) int {
	total := 0
	for _, r := range stripANSI(s) {
		total += wcwidth(r)
	}
	return total
}

// isCJK reports whether r is a CJK ideograph or full-width punctuation rune.
func isCJK(r rune) bool {
	return isCJKAfter(r) ||
		(r >= 0xFF00 && r <= 0xFFEF) ||
		(r >= 0x2F800 && r <= 0x2FA1F)
}

// isCJKAfter reports whether a wrap boundary should open after r (the ranges
// the reference implementation splits after).
func isCJKAfter(r rune) bool {
	return (r >= 0x3000 && r <= 0x303F) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0xF900 && r <= 0xFAFF)
}

func isSpace(r rune) bool {
	return unicode.IsSpace(r)
}

// isAlnumString reports whether the single-rune string is alphanumeric.
func isAlnumString(s string) bool {
	r, n := utf8.DecodeRuneInString(s)
	return n > 0 && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// lstrip mirrors Python str.lstrip (all whitespace, not just spaces).
func lstrip(s string) string {
	return strings.TrimLeft(s, " \t\r\n\v\f")
}

// leadingSpaces returns the number of leading whitespace runes.
func leadingSpaces(s string) int {
	return utf8.RuneCountInString(s) - utf8.RuneCountInString(lstrip(s))
}

// sliceRunes removes the first n runes of s (byte-safe for wide runes).
func sliceRunes(s string, n int) string {
	r := []rune(s)
	if n <= 0 {
		return s
	}
	if n >= len(r) {
		return ""
	}
	return string(r[n:])
}
