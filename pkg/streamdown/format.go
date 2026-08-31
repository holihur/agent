package streamdown

import (
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	supDigits = [10]rune{0x2070, 0x00B9, 0x00B2, 0x00B3, 0x2074, 0x2075, 0x2076, 0x2077, 0x2078, 0x2079}

	linkRe     = regexp.MustCompile(`\[([^\]]+)\]\(([^\)]+)\)`)
	imageRe    = regexp.MustCompile(`!\[([^\]]*)\]\(([^\)]+)\)`)
	footnoteRe = regexp.MustCompile(`\[\^(\d+)\]:?`)

	// Inline tokens: strikethrough, bold+italic combos, 1-3 * / _, backtick
	// runs, or plain runs of anything else.
	inlineTokenRe = regexp.MustCompile(`((~~|\*\*_|_\*\*|\*{1,3}|_{1,3}|\x60+)|[^~_*\x60]+)`)
	wsRe          = regexp.MustCompile(`\s+`)
)

// notText reports whether token is not "word-like": CJK, punctuation, or
// empty. Used to decide whether a * / _ / ~ marker opens a format span.
func notText(token string) bool {
	if cjkCount(token) > 0 {
		return true
	}
	if token == `\` || token == `"` {
		return false
	}
	if token == "" {
		return true
	}
	for _, r := range token {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// lineFormat converts inline markdown (links, footnotes, code, bold, italic,
// underline, strikethrough) into ANSI-styled text. It mutates the renderer's
// inline state, so it is streamed line by line.
func (r *Renderer) lineFormat(line string) string {
	s := r.state
	style := s.Style

	// Images: this port cannot draw into the terminal, so it falls back to the
	// alt text (or the URL) so image captions stay readable.
	if style.Images {
		line = imageRe.ReplaceAllStringFunc(line, func(m string) string {
			g := imageRe.FindStringSubmatch(m)
			if strings.TrimSpace(g[1]) != "" {
				return g[1]
			}
			return g[2]
		})
	}

	if style.Links {
		line = linkRe.ReplaceAllStringFunc(line, func(m string) string {
			g := linkRe.FindStringSubmatch(m)
			return linkOSC[0] + g[2] + "\x1b\\" + style.Link + g[1] + underline[1] + linkOSC[1] + ANSIFGReset
		})
	}

	// Footnotes become superscript digits.
	line = footnoteRe.ReplaceAllStringFunc(line, func(m string) string {
		g := footnoteRe.FindStringSubmatch(m)
		var b strings.Builder
		for _, d := range g[1] {
			if d >= '0' && d <= '9' {
				b.WriteRune(supDigits[d-'0'])
			}
		}
		return b.String()
	})

	var out strings.Builder
	last := 0
	for _, m := range inlineTokenRe.FindAllStringSubmatchIndex(line, -1) {
		start, end := m[0], m[1]
		if start > last {
			out.WriteString(line[last:start])
		}
		last = end

		token := wsRe.ReplaceAllString(line[start:end], " ")
		next := ""
		if end < len(line) {
			_, size := utf8.DecodeRuneInString(line[end:])
			next = line[end : end+size]
		}
		prev := ""
		if start > 0 {
			_, size := utf8.DecodeLastRuneInString(line[:start])
			prev = line[start-size : start]
		}

		switch {
		case strings.Contains(token, "`") && (s.inlineCode == "" || s.inlineCode == token):
			if s.inlineCode != "" {
				if strings.Contains(s.inlineCode, " ") {
					r.savebrace()
				}
				s.inlineCode = ""
			} else {
				s.inlineCode = token
				s.codeBufferRaw = ""
			}
			if s.inlineCode != "" {
				out.WriteString(ANSIBG + style.Mid)
			} else {
				out.WriteString(s.bg)
				s.codeBufferRaw = ""
			}

		case s.inlineCode != "":
			out.WriteString(token)
			s.codeBufferRaw += token

		case token == "~~" && (s.inStrikeout || notText(prev)):
			s.inStrikeout = !s.inStrikeout
			if s.inStrikeout {
				out.WriteString(strikeout[0])
			} else {
				out.WriteString(strikeout[1])
			}

		case (token == "**_" || token == "_**" || token == "___" || token == "***") && (s.inBold || notText(prev)):
			s.inBold = !s.inBold
			if s.inBold {
				out.WriteString(bold[0])
			} else {
				out.WriteString(bold[1])
			}
			s.inItalic = !s.inItalic
			if s.inItalic {
				out.WriteString(italic[0])
			} else {
				out.WriteString(italic[1])
			}

		case (token == "__" || token == "**") && (s.inBold || notText(prev)):
			s.inBold = !s.inBold
			if s.inBold {
				out.WriteString(bold[0])
			} else {
				out.WriteString(bold[1])
			}

		case token == "*" && (s.inItalic || notText(prev)):
			if s.inItalic || (!s.inItalic && next != " ") {
				s.inItalic = !s.inItalic
				if s.inItalic {
					out.WriteString(italic[0])
				} else {
					out.WriteString(italic[1])
				}
			} else {
				out.WriteString(token)
			}

		case token == "_" && (s.inUnderline || (notText(prev) && isAlnumString(next))):
			s.inUnderline = !s.inUnderline
			if s.inUnderline {
				out.WriteString(underline[0])
			} else {
				out.WriteString(underline[1])
			}

		default:
			out.WriteString(token)
		}
	}
	if last < len(line) {
		out.WriteString(line[last:])
	}
	return out.String()
}

// emitH renders a heading at the given level (1-6). Levels 1-2 are centred,
// levels 3+ are coloured.
func (r *Renderer) emitH(level int, text string) string {
	s := r.state
	style := s.Style

	text = r.lineFormat(text)
	wrapped := r.textWrap(text, -1, 0, "", "", false, false)

	var res []string
	for _, line := range wrapped {
		center := float64(s.currentWidth(false)-visibleLength(line)) / 2
		switch level {
		case 1:
			res = append(res, s.spaceLeft(false)+"\n"+s.spaceLeft(false)+bold[0]+
				strings.Repeat(" ", int(math.Floor(center)))+line+bold[1]+"\n")
		case 2:
			res = append(res, s.spaceLeft(false)+"\n"+s.spaceLeft(false)+bold[0]+ANSIFG+style.Bright+
				strings.Repeat(" ", int(math.Floor(center)))+line+
				strings.Repeat(" ", int(math.Ceil(center)))+bold[1]+ANSIFGReset)
		case 3:
			res = append(res, s.spaceLeft(false)+ANSIFG+style.Head+bold[0]+line+bold[1]+ANSIFGReset)
		case 4:
			res = append(res, s.spaceLeft(false)+ANSIFG+style.Symbol+bold[0]+line+bold[1]+ANSIFGReset)
		case 5:
			res = append(res, s.spaceLeft(false)+line+ANSIFGReset)
		default:
			res = append(res, s.spaceLeft(false)+ANSIFG+style.Grey+line+ANSIFGReset)
		}
	}
	return strings.Join(res, "\n")
}

// formatTable renders one table row (a slice of already-split cells) with
// header/body backgrounds and column borders.
func (r *Renderer) formatTable(rowList []string) []string {
	s := r.state
	style := s.Style

	numCols := len(rowList)
	if numCols == 0 {
		return nil
	}
	available := s.currentWidth(false) - numCols*2
	if available < 0 {
		available = 0
	}
	widthBase := available / numCols
	widthMod := available % numCols

	colWidth := make([]int, numCols)
	for i := 0; i < numCols; i++ {
		colWidth[i] = widthBase
		if i < widthMod {
			colWidth[i]++
		}
	}

	bgColor := style.Mid
	if s.inTable != tableHead {
		bgColor = style.Dark
	}
	s.bg = ANSIBG + bgColor

	// First pass: wrap every cell and find the tallest one.
	wrapped := make([][]string, numCols)
	rowHeight := 0
	for ix, row := range rowList {
		cell := r.textWrap(row, colWidth[ix], 0, "", "", true, true)
		if len(cell) == 0 {
			cell = []string{""}
		}
		wrapped[ix] = cell
		rowHeight = max(rowHeight, len(cell))
	}

	// Second pass: emit each visual row.
	var out []string
	for ix := 0; ix < rowHeight; ix++ {
		extra := ""
		if s.inTable != tableHead && ix == rowHeight-1 {
			extra = "\x1b[4;58;2;" + style.Mid // underline-colour bottom border
		}
		segments := make([]string, numCols)
		for iy := 0; iy < numCols; iy++ {
			cell := wrapped[iy]
			segment := ""
			if ix < len(cell) {
				segment = cell[ix]
			}
			pad := colWidth[iy] - visibleLength(segment)
			segments[iy] = ANSIBG + bgColor + extra + " " + segment + strings.Repeat(" ", max(0, pad))
		}
		sep := ANSIBG + bgColor + extra + ANSIFG + style.Symbol + "│" + ANSIReset
		out = append(out, s.spaceLeft(false)+ANSIFGReset+strings.Join(segments, sep)+ANSIReset)
	}

	s.bg = ANSIBGReset
	return out
}

// codeWrap breaks long code lines into chunks that fit the content width.
// It returns the indentation of the original line and the chunks.
func (r *Renderer) codeWrap(textIn string) (int, []string) {
	s := r.state

	if !s.Style.PrettyBroken && s.widthWrap && len(textIn) > s.fullWidth(0) {
		return 0, []string{textIn}
	}

	indent := leadingSpaces(textIn)
	runes := []rune(lstrip(textIn))

	mywidth := s.fullWidth(0) - indent
	if s.Style.PrettyBroken {
		mywidth = s.fullWidth(-4) - indent
	}
	if mywidth <= 0 {
		return indent, []string{textIn}
	}
	if len(runes) == 0 {
		return 0, []string{textIn}
	}

	var res []string
	for i := 0; i < len(runes); i += mywidth {
		res = append(res, string(runes[i:min(i+mywidth, len(runes))]))
	}
	if strings.TrimSpace(res[len(res)-1]) == "" {
		res = res[:len(res)-1]
	}
	return indent, res
}
