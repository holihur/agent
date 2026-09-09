// Command gen-usage 从 `agent -h` 的真实输出生成 cmd/agent/usage.md,
// 保证系统提示词里的 CLI 用法与 flag 定义同步,无需手工维护。
//
// 运行:go generate ./... (cmd/agent/main.go 中的 //go:generate 指令)。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	out := flag.String("out", "cmd/agent/usage.md", "output file")
	flag.Parse()

	// `go run <agent 包> -h` 打印用法后以退出码 2 退出;捕获其输出即可。
	// 用完整 import 路径,生成器从仓库任意目录运行均可。
	cmd := exec.Command("go", "run", "github.com/holihur/agent/cmd/agent", "-h")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil && stderr.Len() == 0 {
		fmt.Fprintln(os.Stderr, "gen-usage:", err)
		os.Exit(1)
	}
	usage := strings.TrimSpace(stderr.String())
	if usage == "" {
		fmt.Fprintln(os.Stderr, "gen-usage: empty usage output")
		os.Exit(1)
	}

	// 跳过 -system:其默认值即系统提示词全文,会递归嵌套(且随文本增长)。
	usage = dropFlag(usage, "-system string")

	// flag 包输出含临时二进制路径(argv[0]),替换为 agent。
	if i := strings.Index(usage, "Usage of "); i >= 0 {
		if j := strings.Index(usage[i:], ":\n"); j >= 0 {
			usage = "Usage of agent" + usage[i+j:]
		}
	}

	content := "# agent CLI 用法\n\n以下是 `agent -h` 的完整 flag 一览(自动生成,勿手改):\n\n```\n" + usage + "\n```\n"
	if err := os.WriteFile(*out, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-usage:", err)
		os.Exit(1)
	}
	fmt.Println("gen-usage: wrote", *out)
}

// dropFlag 从 flag 用法输出中删除一个 flag 条目(两行:名行 + 缩进说明行)。
func dropFlag(usage, nameLine string) string {
	lines := strings.Split(usage, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if lines[i] == "  "+nameLine {
			if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "    \t") {
				i++
			}
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}
