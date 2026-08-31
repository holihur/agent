package streamdown_test

import (
	"fmt"
	"os"
	"strings"

	"github.com/holihur/agent/pkg/streamdown"
)

// ExampleRenderer shows the streaming usage: feed markdown through one Render
// call and let it appear on the terminal as it arrives.
func ExampleRenderer() {
	src := "# Hello\n\nThis is **bold** with `code` and a [link](https://example.com).\n\n" +
		"```go\nfunc main() { println(\"hi\") }\n```\n"

	var out strings.Builder
	r, err := streamdown.New(&out, streamdown.Config{Width: 80})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if err := r.RenderString(src); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	r.Tidyup()

	// The output is ANSI-styled; strip it for a plain view.
	fmt.Printf("rendered %d bytes\n", out.Len())
	fmt.Print(streamdown.StripANSI(out.String()))
}
