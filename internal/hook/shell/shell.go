// Package shell 实现 REPL "!" shell 逃逸:以 "!" 开头的输入直接交给 sh 执行,
// 不进对话历史、不调模型。默认开启,-shell-escape off/none 可禁用。
package shell

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

var shellEscape = flag.String("shell-escape", "on", `REPL "!" shell escape; off/none disables (hook)`)

func init() {
	hook.Register("shell", installShell)
}

// installShell 接上 REPL 的 "!" shell 逃逸:以 "!" 开头的输入直接交给
// sh -c 执行,合并输出作为本轮回答返回,不进对话历史。
func installShell(h *agent.Hooks, _ hook.Deps) error {
	if !shellEscapeEnabled(*shellEscape) {
		return nil
	}
	h.OnInterceptUserInput(runShell)
	return nil
}

// shellEscapeEnabled 判断 -shell-escape 模式是否启用("off"/"none" 禁用)。
func shellEscapeEnabled(mode string) bool {
	return mode != "off" && mode != "none"
}

// shellTimeout 是单条命令的保险丝:拦截器拿不到 Run 的 ctx,无法感知 Ctrl-C。
const shellTimeout = 2 * time.Minute

// runShell 判断输入是否为 "!" 命令;是则执行并把输出作为已处理结果返回。
func runShell(input string) (string, bool) {
	if !strings.HasPrefix(input, "!") {
		return "", false
	}
	cmdText := strings.TrimSpace(input[1:])
	if cmdText == "" {
		return "usage: !<shell command>  (e.g. !git status)", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", cmdText).CombinedOutput()

	var b strings.Builder
	b.WriteString(strings.TrimRight(string(out), "\n"))
	if err != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee) && ee.ExitCode() >= 0:
			fmt.Fprintf(&b, "[exit %d]", ee.ExitCode())
		default:
			fmt.Fprintf(&b, "[error] %s", err)
		}
	}
	if b.Len() == 0 {
		return "(no output)", true
	}
	return b.String(), true
}
