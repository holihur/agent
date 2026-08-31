package streamdown

import (
	"strings"
	"testing"
)

func TestHSVToRGB(t *testing.T) {
	cases := []struct {
		h, s, v float64
		r, g, b int
	}{
		{0, 0, 1, 255, 255, 255},
		{0, 1, 1, 255, 0, 0},
		{1.0 / 3.0, 1, 1, 0, 255, 0},
		{0.8, 0.5, 0.75, 172, 95, 191}, // the reference's Symbol colour
		{0.8, 0.75, 0.125, 27, 7, 31},  // the reference's Dark colour
	}
	for _, tc := range cases {
		rr, gg, bb := hsvToRGB(tc.h, tc.s, tc.v)
		if int(rr*255) != tc.r || int(gg*255) != tc.g || int(bb*255) != tc.b {
			t.Errorf("hsvToRGB(%v, %v, %v) = (%d, %d, %d), want (%d, %d, %d)",
				tc.h, tc.s, tc.v, int(rr*255), int(gg*255), int(bb*255), tc.r, tc.g, tc.b)
		}
	}
}

func TestApplyMultipliers(t *testing.T) {
	// Default palette against the default HSV {0.8, 0.5, 0.5}.
	cfg := DefaultConfig()
	got := applyMultipliers(cfg.Dark, cfg.HSV[0], cfg.HSV[1], cfg.HSV[2])
	if got != "7;17;31m" {
		t.Errorf("Dark = %q, want 7;17;31m", got)
	}
	if applyMultipliers(cfg.Mid, cfg.HSV[0], cfg.HSV[1], cfg.HSV[2]) != "31;44;63m" {
		t.Errorf("Mid wrong")
	}
	if applyMultipliers(cfg.Symbol, cfg.HSV[0], cfg.HSV[1], cfg.HSV[2]) != "95;133;191m" {
		t.Errorf("Symbol wrong")
	}
}

func TestANSI2Hex(t *testing.T) {
	if got := ansi2hex("7;17;31m"); got != "#07111f" {
		t.Errorf("ansi2hex = %q", got)
	}
}

func TestDefaultConfigAndMerge(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Margin != 2 || cfg.ListIndent != 2 {
		t.Errorf("defaults wrong: %+v", cfg)
	}
	if cfg.Links == nil || !*cfg.Links {
		t.Errorf("Links should default to true")
	}
	if cfg.CodeSpaces == nil || *cfg.CodeSpaces {
		t.Errorf("CodeSpaces should default to false")
	}

	// New merges a partial Config over the defaults.
	over := Config{Margin: 6}
	r, err := New(&strings.Builder{}, over)
	if err != nil {
		t.Fatal(err)
	}
	if r.Style.Margin != 6 {
		t.Errorf("margin override ignored: %d", r.Style.Margin)
	}
	if !r.Style.PrettyPad {
		t.Errorf("PrettyPad default lost after merge")
	}
	if !r.Style.Links {
		t.Errorf("Links default lost after merge")
	}
}

func TestNewRejectsMultipleConfigs(t *testing.T) {
	if _, err := New(&strings.Builder{}, Config{}, Config{}); err == nil {
		t.Errorf("New with two configs should fail")
	}
}

func TestStyleDerivedStrings(t *testing.T) {
	r, err := New(&strings.Builder{}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Style.Codebg, "\x1b[48;2;") {
		t.Errorf("Codebg wrong: %q", r.Style.Codebg)
	}
	if !strings.Contains(r.Style.Blockquote, "│") {
		t.Errorf("Blockquote missing bar: %q", r.Style.Blockquote)
	}
	if r.Style.MarginSpaces != "  " {
		t.Errorf("MarginSpaces = %q", r.Style.MarginSpaces)
	}
}

func TestWidthCalc(t *testing.T) {
	r, err := New(&strings.Builder{}, Config{Width: 100, Margin: 4})
	if err != nil {
		t.Fatal(err)
	}
	if r.state.widthFull != 100 {
		t.Errorf("widthFull = %d", r.state.widthFull)
	}
	if r.state.width != 92 {
		t.Errorf("content width = %d, want 92", r.state.width)
	}
}

func TestTidyup(t *testing.T) {
	var buf strings.Builder
	r, err := New(&buf, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	_ = r.RenderString("# hi\n")
	r.Tidyup()
	if !strings.HasSuffix(buf.String(), ANSIReset) {
		t.Errorf("Tidyup did not reset: %q", buf.String())
	}
}

func TestNewInvalidPrompt(t *testing.T) {
	bad := DefaultConfig()
	bad.Prompt = "[unclosed"
	if _, err := New(&strings.Builder{}, bad); err == nil {
		t.Errorf("New with invalid Prompt should return an error")
	}
}
