// Package slashcmd 实现 REPL "/" 命令:以 "/" 开头的输入按命令处理,
// 命令结果作为本轮回答返回,不进对话历史、不调模型。默认开启,
// -slashcmd off/none 可禁用。"/" 前缀与 shell 逃逸 "!" 前缀一样是
// REPL 层面的保留输入形态,正常提问(不以 / 开头)不受影响。
package slashcmd

import (
	"flag"
	"fmt"
	"strings"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

var slashCmd = flag.String("slashcmd", "on", `REPL "/" commands, e.g. /help; off/none disables (hook)`)

func init() {
	hook.Register("slashcmd", installSlash)
}

// installSlash 接上 REPL 的 "/" 命令拦截。
func installSlash(h *agent.Hooks, _ hook.Deps) error {
	if !enabled(*slashCmd) {
		return nil
	}
	h.OnInterceptUserInput(runSlash)
	return nil
}

// enabled 判断 -slashcmd 模式是否启用("off"/"none" 禁用)。
func enabled(mode string) bool {
	return mode != "off" && mode != "none"
}

// helpText 是 /help 的帮助文档。"/exit /quit" 由 CLI 直接处理(REPL 循环
// 控制,不在拦截器内);"!" 逃逸由 shell hook 提供,这里一并列入便于查询。
// 编辑快捷键对应 chzyer/readline 默认绑定(本 REPL 未开启 VimMode,
// 未配置补全器,故不列 Tab/vi 键)。Ctrl-S 正向搜索可能被终端流控吞掉;
// Alt-T/PgUp/PgDn 在库内映射或处理缺失,实际无效,故不列。
const helpText = `REPL commands:
  /help, /h        print this help
  /exit, /quit     exit the REPL (bare exit/quit also work)
  !<cmd>           run a shell command, e.g. !git status

Input starting with "/" is treated as a command; anything else goes to the model.

Editing keys (readline):
  Ctrl-A / Ctrl-E     line start / end
  Ctrl-B / Ctrl-F     move left / right
  Ctrl-P / Ctrl-N     history previous / next
  Ctrl-R / Ctrl-S     history search backward / forward
  Ctrl-U / Ctrl-K     delete to line start / end
  Ctrl-W              delete word backward
  Ctrl-L              clear screen
  Ctrl-C              clear current line
  Ctrl-D              delete char; empty line = exit
  Ctrl-T              transpose chars
  Ctrl-Y              yank (paste deleted text)
  Ctrl-Z              suspend process (fg to resume)
  Alt-B / Alt-F       word back / forward
  Alt-D               delete word forward
  Home / End          line start / end
  Del                 delete char forward
  Arrow keys          move cursor / history`

// runSlash 判断输入是否为 "/" 命令:是则返回处理结果并标记已处理,
// 否则放行给模型。未知命令给出提示,提示指向 /help。
func runSlash(input string) (string, bool) {
	if !strings.HasPrefix(input, "/") {
		return "", false
	}
	switch strings.TrimSpace(input) {
	case "/help", "/h":
		return helpText, true
	}
	return fmt.Sprintf("unknown command: %s (try /help)", strings.TrimSpace(input)), true
}
