package streamdown

import (
	"os"
	"testing"
)

// TestRenderMarkdownDemo 渲染 markdown-demo.md 并原样打印(带 ANSI 颜色),
// 用于人工查看各类语法的终端渲染效果。go test -run TestRenderMarkdownDemo -v
func TestRenderMarkdownDemo(t *testing.T) {
	data, err := os.ReadFile("testdata/markdown-demo.md")
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Width = 100
	r, err := New(os.Stdout, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RenderString(string(data)); err != nil {
		t.Fatal(err)
	}
	r.Tidyup()
}
