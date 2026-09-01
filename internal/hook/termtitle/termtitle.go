// Package termtitle 实现终端标题 hook:每轮用户输入经清洗后写入终端窗口
// 标题(OSC 0),便于多标签页里一眼看到最近一次在问什么。默认开启,
// -term-title off/none 可禁用。标题经 /dev/tty 直写,不污染可能被重定向的
// stdout/stderr;无控制终端(如 CI)时静默跳过。
package termtitle

import (
	"flag"
	"os"
	"strings"
	"unicode"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

var termTitle = flag.String("term-title", "on", `write the latest user input to the terminal title (OSC 0 via /dev/tty); off/none disables (hook)`)

func init() {
	hook.Register("term-title", installTermTitle)
}

// maxTitleLen 是标题的最大 rune 数,超长以省略号截断。
const maxTitleLen = 80

// installTermTitle 把"用户输入 → 终端标题"挂到 RunStart 观测钩子:
// 每轮 Run(含 REPL 与 -q)开头,以当轮原始输入更新标题。
func installTermTitle(h *agent.Hooks, _ hook.Deps) error {
	if !enabled(*termTitle) {
		return nil
	}
	h.OnRunStart(func(in agent.UserInput) {
		title := sanitize(in.Text)
		if title != "" {
			writeTitle(title)
		}
	})
	return nil
}

// enabled 判断 -term-title 模式是否启用("off"/"none" 禁用)。
func enabled(mode string) bool {
	return mode != "off" && mode != "none"
}

// sanitize 把输入整理成安全的标题文本:控制字符(ESC/BEL 等)一律丢弃,
// 防止用户输入伪造 OSC 序列;换行类压成空格;连续空白折叠;超长截断。
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// 其余控制字符丢弃:标题必须是单行纯文本,含 ESC/BEL 会被终端解释
		default:
			b.WriteRune(r)
		}
	}
	title := strings.Join(strings.Fields(b.String()), " ")
	runes := []rune(title)
	if len(runes) > maxTitleLen {
		runes = append(runes[:maxTitleLen-1], '…')
	}
	return string(runes)
}

// writeTitle 写标题;包级变量仅为测试注入出口,生产直写控制终端。
var writeTitle = func(title string) {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return // 无控制终端(重定向/CI):静默跳过
	}
	defer tty.Close()
	_, _ = tty.WriteString("\x1b]0;" + title + "\x07")
}
