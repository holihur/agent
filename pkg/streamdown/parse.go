package streamdown

import (
	"bufio"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	blockRe      = regexp.MustCompile(`^\s*((>\s*)+|[◁<].?think[>▷])(.*)`)
	codeFenceRe  = regexp.MustCompile("^\\s*(```|<pre>)\\s*([^\\s]+|$)\\s*$")
	spacesCodeRe = regexp.MustCompile(`^    \s*[^\s\*]`)
	tableLineRe  = regexp.MustCompile(`^\s*\|.+\|\s*$`)
	tableSepRe   = regexp.MustCompile(`^[\s|:-]+$`)
	listItemRe   = regexp.MustCompile(`^(\s*)([\+*\-] |\+\-+|\d+\.\s+)(.*)`)
	headerRe     = regexp.MustCompile(`^\s*(#{1,6})\s*(.*)`)
	hrRe         = regexp.MustCompile(`^[\s]*([-\*=_]){3,}[\s]*$`)
)

// parse is the streaming markdown state machine. It reads input byte by byte
// and calls emit with each rendered chunk as soon as it is available, so
// partial output appears while the input is still streaming in.
func (r *Renderer) parse(input io.Reader, emit func(string)) {
	s := r.state
	br := bufio.NewReader(input)
	var lastLineEmptyCache bool
	var hl *highlighter

	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			if len(s.buffer) == 0 {
				break
			}
			b = '\n' // flush the final unterminated line
		} else if err != nil {
			break
		}

		s.buffer = append(s.buffer, b)
		if b != '\n' {
			continue
		}

		line := strings.ReplaceAll(string(s.buffer), "\t", "  ")
		s.hasNewline = strings.HasSuffix(line, "\n")

		// Streaming prompt flush: a partial line that looks like a prompt is
		// echoed immediately instead of waiting for the next newline.
		s.maybePrompt = !s.hasNewline && s.currentNone() && r.promptRe != nil && r.promptRe.MatchString(stripANSI(line))
		if s.maybePrompt {
			s.emitFlag = EmitFlush
			emit(line)
			s.currentLine = ""
			s.buffer = s.buffer[:0]
		}
		if !s.hasNewline {
			continue
		}
		s.buffer = s.buffer[:0]

		// --- blockquotes and <think> blocks --------------------------------
		inBlock := false
		if s.inCode == CodeNone {
			if m := blockRe.FindStringSubmatch(line); m != nil {
				inBlock = true
				g1 := m[1]
				switch {
				case len(g1) >= 7 && g1[1:7] == "/think":
					line = ""
					s.blockDepth = 0
					emit(ANSIReset)
				case len(g1) >= 6 && g1[1:6] == "think":
					line = m[3]
					s.blockDepth = 1
					s.blockType = "think"
				default:
					s.blockDepth = strings.Count(g1, ">")
					s.blockType = ">"
					line = line[len(g1):]
				}
			}
		}
		if !inBlock && s.blockType == ">" && s.blockDepth > 0 {
			emit(ANSIFGReset)
			s.blockDepth = 0
		}

		// --- collapse multiple blank lines (not inside code) ---------------
		if s.inCode == CodeNone {
			if strings.TrimSpace(line) == "" {
				if s.lastLineEmpty {
					continue
				}
				s.lastLineEmpty = true
				emit(s.spaceLeft(false))
				continue
			}
			lastLineEmptyCache = s.lastLineEmpty
			s.lastLineEmpty = false
		}

		// --- list state reset ----------------------------------------------
		if !s.inList && len(s.orderedListNumbers) > 0 {
			s.orderedListNumbers[0] = 0
		} else if !strings.HasPrefix(line, strings.Repeat(" ", s.listIndentText)) && strings.TrimSpace(line) != "" {
			s.inList = false
			s.listIndentText = 0
		}

		// --- strip the document's common first-line indent -----------------
		if s.firstIndent == nil {
			v := leadingSpaces(line)
			s.firstIndent = &v
		}
		if leadingSpaces(line) >= *s.firstIndent {
			line = line[*s.firstIndent:]
		}

		// --- leave table mode on non-table lines ---------------------------
		if s.inTable != tableOff && s.inCode == CodeNone && !tableLineRe.MatchString(line) {
			s.inTable = tableOff
		}

		// --- open a code block ---------------------------------------------
		if s.inCode == CodeNone {
			if m := codeFenceRe.FindStringSubmatch(line); m != nil {
				s.inCode = CodeBacktick
				s.codeIndent = leadingSpaces(line)
				s.codeLanguage = m[2]
				if s.codeLanguage == "" {
					s.codeLanguage = "Bash"
				}
			} else if r.Style.CodeSpaces && lastLineEmptyCache && !s.inList {
				if spacesCodeRe.MatchString(line) {
					s.inCode = CodeSpaces
					s.codeLanguage = "Bash"
				}
			}

			if s.inCode != CodeNone {
				s.codeBuffer = ""
				s.codeBufferRaw = ""
				s.codeFirstLine = true
				s.bg = ANSIBG + r.Style.Dark
				if r.Style.PrettyPad || r.Style.PrettyBroken {
					if !r.Style.PrettyPad {
						emit("")
					}
					emit(r.Style.Codepad[0])
				} else {
					emit("")
				}
				if s.inCode == CodeBacktick {
					continue
				}
			}
		}

		// --- code block body ------------------------------------------------
		if s.inCode != CodeNone {
			ended := false
			if (s.inCode == CodeBacktick && (strings.TrimSpace(line) == "```" || strings.TrimSpace(line) == "</pre>")) ||
				(r.Style.CodeSpaces && s.inCode == CodeSpaces && !strings.HasPrefix(line, "    ")) {
				r.savebrace()
				s.codeLanguage = ""
				s.codeIndent = 0
				codeType := s.inCode
				s.inCode = CodeNone
				s.bg = ANSIBGReset
				if r.Style.PrettyPad || r.Style.PrettyBroken {
					emit(r.Style.Codepad[1])
					if !r.Style.PrettyPad {
						emit("")
					}
				} else {
					emit(ANSIReset)
				}
				if codeType == CodeBacktick {
					continue
				}
				ended = true // 4-space blocks fall through to normal handling
			}

			if !ended {
				if s.codeFirstLine || hl == nil {
					s.codeFirstLine = false
					hl = newHighlighter(s.codeLanguage, r.Style.Syntax)
					if strings.HasPrefix(line, strings.Repeat(" ", s.codeIndent)) {
						line = line[s.codeIndent:]
					}
				} else if strings.HasPrefix(line, strings.Repeat(" ", s.codeIndent)) {
					line = line[s.codeIndent:]
				}

				s.codeBufferRaw += line
				s.codeLine += line
				if !strings.HasSuffix(s.codeLine, "\n") {
					continue // wait for the rest of the line
				}
				line = s.codeLine
				s.codeLine = ""

				indent, wrapped := r.codeWrap(line)
				pre := [2]string{"", ""}
				if r.Style.PrettyBroken {
					pre = [2]string{s.spaceLeft(true), "  "}
				}

				for _, tline := range wrapped {
					// Re-highlight the accumulated buffer plus this wrapped
					// chunk, then emit only the newly-highlighted tail so
					// streaming lines keep consistent colours.
					highlighted := hl.Highlight(s.codeBuffer + tline)
					parts := splitUp(highlighted)
					for i := range parts {
						if parts[i] == "\x1b[39m" || parts[i] == "\x1b[49m" || parts[i] == "\x1b[0m" {
							parts[i] = ANSIFmtReset
						}
					}
					for len(parts) > 0 && (parts[len(parts)-1] == ANSIFGReset || parts[len(parts)-1] == ANSIFmtReset) {
						parts = parts[:len(parts)-1]
					}

					tlineLen := visibleLength(strings.TrimRight(tline, "\r\n"))
					ttl := 0
					i := len(parts) - 1
					for ; i > 0; i-- {
						idx := parts[i]
						if len(idx) == 0 {
							continue
						}
						if idx[0] != '\x1b' {
							ttl += utf8.RuneCountInString(idx)
						}
						if ttl > tlineLen {
							break
						}
					}

					newLen := visibleLength(strings.Join(parts[i:], ""))
					snipfrom := newLen - utf8.RuneCountInString(tline) + 2
					if snipfrom == 1 {
						snipfrom = 0
					}
					if snipfrom > 0 && i < len(parts) && parts[i][0] != '\x1b' {
						parts[i] = sliceRunes(parts[i], snipfrom)
					}

					s.codeBuffer += tline
					thisBatch := strings.Join(parts[i:], "")
					thisBatch = strings.TrimPrefix(thisBatch, ANSIFGReset)
					thisBatch = stripStyled(thisBatch)
					for i-1 >= 0 && len(parts[i-1]) > 0 && parts[i-1][0] == '\x1b' {
						thisBatch = parts[i-1] + thisBatch
						i--
					}

					codeLine := strings.Repeat(" ", indent) + thisBatch
					margin := s.fullWidth(-len(pre[1])) - visibleLength(codeLine)%s.widthFull
					emit(pre[0] + r.Style.Codebg + pre[1] + codeLine + ANSIFmtReset +
						strings.Repeat(" ", max(0, margin)) + ANSIBGReset)
				}
				continue
			}
		}

		// --- tables ----------------------------------------------------------
		if s.inCode == CodeNone && tableLineRe.MatchString(line) {
			trimmed := strings.Trim(strings.TrimSpace(line), "|")
			raw := strings.Split(trimmed, "|")
			cells := make([]string, len(raw))
			for i, c := range raw {
				cells[i] = strings.TrimSpace(c)
			}

			if s.inTable == tableOff {
				s.inTable = tableHead
			} else if s.inTable == tableHead {
				if !tableSepRe.MatchString(line) {
					// malformed separator: reference logs a warning, we ignore
				}
				s.inTable = tableBody
				continue
			}

			for _, row := range r.formatTable(cells) {
				emit(row)
			}
			continue
		}

		// --- lists ------------------------------------------------------------
		content := line
		bullet := " "
		if m := listItemRe.FindStringSubmatch(line); m != nil {
			s.listIndentText = utf8.RuneCountInString(m[2]) - 1
			s.inList = true

			indent := utf8.RuneCountInString(m[1])
			listType := "bullet"
			if len(m[2]) > 0 && m[2][0] >= '0' && m[2][0] <= '9' {
				listType = "number"
			}
			content = m[3]

			for len(s.listItemStack) > 0 && s.listItemStack[len(s.listItemStack)-1].Indent > indent {
				s.listItemStack = s.listItemStack[:len(s.listItemStack)-1]
				if len(s.orderedListNumbers) > 0 {
					s.orderedListNumbers = s.orderedListNumbers[:len(s.orderedListNumbers)-1]
				}
			}
			if len(s.listItemStack) > 0 && s.listItemStack[len(s.listItemStack)-1].Indent < indent {
				s.listItemStack = append(s.listItemStack, ListItem{Indent: indent, Type: listType})
				s.orderedListNumbers = append(s.orderedListNumbers, 0)
			} else if len(s.listItemStack) == 0 {
				s.listItemStack = append(s.listItemStack, ListItem{Indent: indent, Type: listType})
				s.orderedListNumbers = append(s.orderedListNumbers, 0)
			}
			if listType == "number" {
				s.orderedListNumbers[len(s.orderedListNumbers)-1]++
			}

			bullet = "•"
			if listType == "number" {
				n := s.orderedListNumbers[len(s.orderedListNumbers)-1]
				if p, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(m[2], ".")), 64); err == nil {
					n = int(math.Max(float64(n), p))
				}
				bullet = strconv.Itoa(n)
			}
		}

		if s.inList {
			indent := (len(s.listItemStack) - 1) * r.Style.ListIndent
			wrapWidth := s.currentWidth(true) - r.Style.ListIndent
			wrapped := r.textWrap(content, wrapWidth, r.Style.ListIndent,
				strings.Repeat(" ", indent)+ANSIFG+r.Style.Symbol+bullet+ANSIReset+" ",
				strings.Repeat(" ", indent), false, false)
			for _, wl := range wrapped {
				out := wl
				if s.blockDepth > 0 {
					out = stripANSI(wl)
				}
				emit(s.spaceLeft(false) + out + "\n")
			}
			continue
		}

		// --- headings ----------------------------------------------------------
		if m := headerRe.FindStringSubmatch(line); m != nil {
			level := utf8.RuneCountInString(m[1])
			emit(r.emitH(level, m[2]))
			continue
		}

		// --- horizontal rules / setext headings --------------------------------
		if m := hrRe.FindStringSubmatch(line); m != nil {
			if s.lastLineEmpty || lastLineEmptyCache {
				emit(r.Style.MarginSpaces + ANSIFG + r.Style.Symbol + strings.Repeat("─", s.width) + ANSIReset)
			} else {
				if strings.Contains(m[1], "-") {
					s.emitFlag = EmitH1
				} else {
					s.emitFlag = EmitH2
				}
				emit("")
			}
			continue
		}

		// --- normal text ----------------------------------------------------------
		s.listItemStack = nil
		if len(line) == 0 {
			emit("")
		}
		if visibleLength(line) < s.width {
			emit(s.spaceLeft(false) + r.lineFormat(lstrip(line)))
		} else {
			for _, wl := range r.textWrap(line, -1, 0, "", "", false, false) {
				emit(s.spaceLeft(false) + wl + "\n")
			}
		}
	}
}
