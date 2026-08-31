package streamdown

import (
	"fmt"
	"math"
	"strings"
)

// ANSI escape sequences used by the renderer. Palette colours are emitted as
// true-colour SGR sequences; the constants below are their fixed prefixes.
const (
	ANSIFG       = "\x1b[38;2;"
	ANSIBG       = "\x1b[48;2;"
	ANSIReset    = "\x1b[0m"
	ANSIFGReset  = "\x1b[39m"
	ANSIFmtReset = "\x1b[24;23;22m" // underline/italic/bold reset (keeps fg/bg)
	ANSIBGReset  = "\x1b[49m"
)

var (
	bold      = [2]string{"\x1b[1m", "\x1b[22m"}
	underline = [2]string{"\x1b[4m", "\x1b[24m"}
	italic    = [2]string{"\x1b[3m", "\x1b[23m"}
	strikeout = [2]string{"\x1b[9m", "\x1b[29m"}
	linkOSC   = [2]string{"\x1b]8;;", "\x1b]8;;\x1b\\"} // OSC 8 hyperlink open/close
)

// ColorMult is the HSV multiplier applied on top of the base hue (H),
// saturation (S) and value (V) to derive one palette entry.
type ColorMult struct {
	H float64
	S float64
	V float64
}

// Style holds the derived colour palette and layout options. It is exported so
// callers can inspect or tweak it between renders, but the normal way to
// configure a renderer is Config.
type Style struct {
	// Palette entries are ANSI true-colour parameter strings ("R;G;B;m"),
	// meant to be appended directly after ANSIFG / ANSIBG.
	Dark   string
	Mid    string
	Symbol string
	Head   string
	Grey   string
	Bright string

	Margin       int
	ListIndent   int
	PrettyPad    bool
	PrettyBroken bool
	Syntax       string
	Plaintext    bool

	CodeSpaces bool
	Images     bool
	Links      bool

	// Derived strings.
	Codebg       string
	Link         string
	Blockquote   string
	MarginSpaces string
	Codepad      [2]string
}

// hsvToRGB ports Python's colorsys.hsv_to_rgb; all arguments are in [0, 1].
func hsvToRGB(h, s, v float64) (float64, float64, float64) {
	if s == 0 {
		return v, v, v
	}
	i := int(h * 6)
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - s*f)
	t := v * (1 - s*(1-f))
	switch i % 6 {
	case 0:
		return v, t, p
	case 1:
		return q, v, p
	case 2:
		return p, v, t
	case 3:
		return p, q, v
	case 4:
		return t, p, v
	default:
		return v, p, q
	}
}

// applyMultipliers scales the base HSV by the entry's multipliers, clamps to
// [0, 1] and formats the result as an ANSI parameter string ("R;G;B;m").
func applyMultipliers(m ColorMult, h, s, v float64) string {
	r, g, b := hsvToRGB(
		math.Min(1, h*m.H),
		math.Min(1, s*m.S),
		math.Min(1, v*m.V),
	)
	return fmt.Sprintf("%d;%d;%dm", int(r*255), int(g*255), int(b*255))
}

// ansi2hex converts an ANSI parameter string ("R;G;B;m") to "#rrggbb".
func ansi2hex(code string) string {
	parts := strings.Split(strings.TrimSuffix(code, "m"), ";")
	if len(parts) != 3 {
		return "#202020"
	}
	var v [3]int
	for i, p := range parts {
		fmt.Sscanf(p, "%d", &v[i])
	}
	return fmt.Sprintf("#%02x%02x%02x", v[0], v[1], v[2])
}
