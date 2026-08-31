package streamdown

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// splitUp splits a highlighted string into alternating ANSI escapes and plain
// runs (mirrors the reference implementation's split_up lambda).
var splitUpRe = regexp.MustCompile(`(\x1b[^m]*m|[^\x1b]*)`)

func splitUp(s string) []string {
	parts := splitUpRe.FindAllString(s, -1)
	// Drop empty segments (the [^\x1b]* alternative can match "" at the end
	// and between escapes), keeping behaviour close to Python's findall.
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cjkCount counts CJK ideographs / full-width runes in s (ANSI stripped).
func cjkCount(s string) int {
	n := 0
	for _, r := range stripANSI(s) {
		if isCJK(r) {
			n++
		}
	}
	return n
}

// splitText tokenises text for wrapping: whitespace separates words, and CJK
// runes split at every character boundary so wide text wraps mid-sentence.
// This is a hand-rolled equivalent of the reference implementation's regex
// (Go's RE2 has no lookbehind/lookahead).
func splitText(text string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range text {
		switch {
		case isSpace(r):
			flush()
		case isCJKAfter(r):
			cur = append(cur, r)
			flush()
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// sgrClass buckets an SGR escape sequence by the class of its first parameter,
// matching the alternation order of the reference implementation
// (fg > bg > bold > italic > underline > reset).
type sgrClass int

const (
	sgrNone sgrClass = iota
	sgrFG
	sgrBG
	sgrBold
	sgrItalic
	sgrUnderline
	sgrReset
)

var (
	sgrParamRe = regexp.MustCompile(`^\x1b\[([0-9;]*)m$`)
	sgrFgRe    = regexp.MustCompile(`^3\d`)
	sgrBgRe    = regexp.MustCompile(`^4\d`)
	sgrBoldRe  = regexp.MustCompile(`^2?[12]`)
	sgrItaRe   = regexp.MustCompile(`^2?3`)
	sgrUndRe   = regexp.MustCompile(`^3?2`)
)

func sgrClassOf(code string) sgrClass {
	m := sgrParamRe.FindStringSubmatch(code)
	if m == nil {
		return sgrNone
	}
	params := strings.Split(m[1], ";")
	if len(params) == 0 || params[0] == "" {
		return sgrNone
	}
	p := params[0]
	switch {
	case sgrFgRe.MatchString(p):
		return sgrFG
	case sgrBgRe.MatchString(p):
		return sgrBG
	case sgrBoldRe.MatchString(p):
		return sgrBold
	case sgrItaRe.MatchString(p):
		return sgrItalic
	case sgrUndRe.MatchString(p):
		return sgrUnderline
	case p == "0":
		return sgrReset
	}
	return sgrNone
}

// stripStyled removes leading and trailing whitespace from s, treating ANSI
// escape sequences as transparent: they neither block the strip nor are
// removed. The reference implementation relies on plain str.strip() here, but
// chroma interleaves escape sequences between the boundary newlines of a
// highlighted block, so a naive strip would leave embedded newlines behind.
func stripStyled(s string) string {
	// leading: skip whitespace runes, keep any interleaved escapes
	i := 0
	prefix := ""
	for i < len(s) {
		if n := ansiEscapeLenAt(s[i:]); n > 0 {
			prefix += s[i : i+n]
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		break
	}
	s = prefix + s[i:]

	// trailing: drop whitespace at the very end only
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if !unicode.IsSpace(r) {
			break
		}
		s = s[:len(s)-size]
	}
	return s
}

// ansiEscapeLenAt reports the length of an ANSI escape sequence starting at
// the beginning of s, or 0 if s does not start with an escape.
func ansiEscapeLenAt(s string) int {
	if len(s) == 0 || s[0] != '\x1b' {
		return 0
	}
	for _, re := range []*regexp.Regexp{sgrEscapeRe, ansiEscapeRe} {
		if loc := re.FindStringIndex(s); loc != nil && loc[0] == 0 {
			return loc[1]
		}
	}
	return 0
}
func ansiCollapse(codelist, inp []string) []string {
	for _, code := range inp {
		cls := sgrClassOf(code)
		if cls == sgrReset {
			return inp
		}
		if cls == sgrNone {
			continue
		}
		kept := codelist[:0]
		for _, c := range codelist {
			if sgrClassOf(c) != cls {
				kept = append(kept, c)
			}
		}
		codelist = kept
	}
	return append(codelist, inp...)
}

// textWrap wraps text to width columns. It formats inline markdown first, so
// it must run with the renderer's inline state. indent applies a hanging
// indent to continuation lines. If forceTruncate is set, overflowing lines are
// cut with an ellipsis (tables).
func (r *Renderer) textWrap(text string, width, indent int, firstLinePrefix, subsequentLinePrefix string, forceTruncate, preserveFormat bool) []string {
	s := r.state
	if width < 0 {
		width = s.width
	}

	formatted := r.lineFormat(text)
	words := splitText(formatted)
	words = append(words, "") // trailing empty word flushes the final line

	var lines []string
	currentLine := ""
	var currentStyle []string
	resetter := ""
	if !preserveFormat {
		resetter = ANSIFmtReset
	}

	oldWord := ""
	for _, word := range words {
		codes := extractANSICodes(word)
		if len(codes) > 0 && strings.HasPrefix(word, codes[0]) {
			currentStyle = append(currentStyle, codes[0])
			codes = codes[1:]
		}

		if len(word) > 0 && visibleLength(currentLine)+visibleLength(word)+1 <= width {
			space := ""
			// The reference uses the character count of the visible word here,
			// not the display width (words of only control characters still
			// count as non-empty).
			if utf8.RuneCountInString(stripANSI(word)) > 0 && currentLine != "" {
				space = " "
			}
			// No space between CJK characters, and none before a colon.
			if (strings.Contains(stripANSI(word), ":") || cjkCount(word) > 0) && cjkCount(oldWord) > 0 {
				space = ""
			}
			currentLine += space + word
		} else {
			prefix := firstLinePrefix
			if len(lines) > 0 {
				prefix = subsequentLinePrefix
			}
			lineContent := prefix + currentLine
			for forceTruncate && visibleLength(lineContent) >= width {
				r := []rune(lineContent)
				if len(r) <= 2 {
					break
				}
				lineContent = string(r[:len(r)-2]) + "…"
			}
			margin := max(0, width-visibleLength(lineContent))

			if strings.TrimSpace(lineContent) != "" {
				// Make sure an open OSC 8 hyperlink is closed on this line.
				if strings.Contains(lineContent, linkOSC[0]) {
					lineContent += linkOSC[1]
				}
				lines = append(lines, lineContent+resetter+s.bg+strings.Repeat(" ", margin))
			}

			currentLine = strings.Repeat(" ", indent) + strings.Join(currentStyle, "") + word
		}

		if len(codes) > 0 {
			currentStyle = append(currentStyle, codes...)
			currentStyle = ansiCollapse(currentStyle, codes)
		}
		oldWord = word
	}

	if len(lines) < 1 {
		return nil
	}
	if len(lines) == 1 {
		lines[0] = strings.TrimRight(lines[0], " ")
	}
	return lines
}
