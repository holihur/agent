package streamdown

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
)

// trickleReader wraps a byte slice and returns at most size bytes per Read,
// simulating a slow stream (e.g. an LLM token stream) inside one Render call.
type trickleReader struct {
	data []byte
	off  int
	size int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := min(r.size, len(r.data)-r.off, len(p))
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

// TestStreamingChunkEquivalence feeds the fixture to one Render call through
// a reader that yields tiny chunks. The reference renderer emits output as
// soon as a line completes, so the streaming result must be byte-identical to
// a single-shot render.
func TestStreamingChunkEquivalence(t *testing.T) {
	src := []byte(readFixture(t, "cjk.md"))
	want := renderFixture(t, "cjk.md", 60, true)
	for _, size := range []int{1, 2, 7, 16, 64} {
		t.Run("chunk"+strconv.Itoa(size), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Width = 60
			cfg.Plaintext = true
			var buf bytes.Buffer
			r, err := New(&buf, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Render(&trickleReader{data: src, size: size}); err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("chunk size %d produced different output\n--- got ---\n%s\n--- want ---\n%s", size, buf.Bytes(), want)
			}
		})
	}
}

// TestStreamingLineByLine feeds one line at a time, mirroring how an LLM
// token stream arrives with natural line boundaries.
func TestStreamingLineByLine(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Width = 80
	var buf bytes.Buffer
	r, err := New(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Join([]string{
		"# Title\n",
		"Some ",
		"**bold** ",
		"text here\n",
		"```go\n",
		"func main() {\n",
		"\tprintln(\"hi\")\n",
		"}\n",
		"```\n",
	}, "")
	if err := r.Render(&trickleReader{data: []byte(data), size: 6}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(stripANSI(out), "bold") {
		t.Errorf("streamed output missing formatted text: %q", out)
	}
	if !strings.Contains(stripANSI(out), "println") {
		t.Errorf("streamed output missing code: %q", out)
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
