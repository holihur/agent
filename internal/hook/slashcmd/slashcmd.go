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
	"github.com/holihur/agent/internal/permission"
)

var slashCmd = flag.String("slashcmd", "on", `REPL "/" commands, e.g. /help; off/none disables (hook)`)

// permCWD 保存当前项目根，供斜杠命令读写权限文件。
var permCWD string

func init() {
	hook.Register("slashcmd", installSlash)
}

// installSlash 接上 REPL 的 "/" 命令拦截。
func installSlash(h *agent.Hooks, d hook.Deps) error {
	if !enabled(*slashCmd) {
		return nil
	}
	permCWD = d.CWD
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
  /new             start a new session (clears history; with -session rotates to a fresh name, old kept)
  /allow <pat> [| <inputPat>] [--global]  persist allow rule (wildcard * ?); e.g. /allow fs__* , /allow read | *"/tmp/*"*
  /deny <pat> [| <inputPat>] [--global]   persist deny rule (wildcard * ?)
  /allow-list, /perm list                 list persisted permissions (project + global merged)
  /revoke <pat> [--global]                remove persisted rule
  /perm help       permission help
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
	trim := strings.TrimSpace(input)
	switch trim {
	case "/help", "/h":
		return helpText, true
	case "/allow-list", "/perm list", "/perms", "/permissions":
		return runPermList(), true
	case "/perm help", "/allow help":
		return permHelpText, true
	}
	if strings.HasPrefix(trim, "/allow ") || trim == "/allow" {
		return runAllow(strings.TrimSpace(strings.TrimPrefix(trim, "/allow"))), true
	}
	if strings.HasPrefix(trim, "/deny ") || trim == "/deny" {
		return runDeny(strings.TrimSpace(strings.TrimPrefix(trim, "/deny"))), true
	}
	if strings.HasPrefix(trim, "/revoke ") || trim == "/revoke" {
		return runRevoke(strings.TrimSpace(strings.TrimPrefix(trim, "/revoke"))), true
	}
	if strings.HasPrefix(trim, "/perm ") {
		rest := strings.TrimSpace(strings.TrimPrefix(trim, "/perm"))
		switch {
		case rest == "list" || rest == "ls":
			return runPermList(), true
		case strings.HasPrefix(rest, "allow "):
			return runAllow(strings.TrimSpace(strings.TrimPrefix(rest, "allow"))), true
		case strings.HasPrefix(rest, "deny "):
			return runDeny(strings.TrimSpace(strings.TrimPrefix(rest, "deny"))), true
		case strings.HasPrefix(rest, "revoke "):
			return runRevoke(strings.TrimSpace(strings.TrimPrefix(rest, "revoke"))), true
		case rest == "help":
			return permHelpText, true
		}
	}
	return fmt.Sprintf("unknown command: %s (try /help)", trim), true
}

const permHelpText = `Permission commands:
  /allow <pattern> [| <inputPattern>] [--global]
    pattern 支持 * ? 通配，匹配工具名如 fs__* / *read / *
    inputPattern 可选，通配匹配工具输入 JSON 字符串，如 *"/tmp/*"* 
    示例:
      /allow fs__read
      /allow shell__* 
      /allow "*" 
      /allow read | *"/tmp/*"*
      /allow fs__* --global   写入全局 ~/.config/agent/permissions.json
  /deny <pattern> [| <inputPattern>] [--global]   持久拒绝（deny 优先于 allow）
  /allow-list  列出已持久化规则（合并全局与项目）
  /revoke <pattern> [--global]  移除规则，支持通配批量删除`

func runAllow(args string) string {
	if args == "" {
		return "usage: /allow <pattern> [| <inputPattern>] [--global]"
	}
	global := false
	if strings.Contains(args, "--global") {
		global = true
		args = strings.ReplaceAll(args, "--global", "")
		args = strings.ReplaceAll(args, "-g", "")
		args = strings.TrimSpace(args)
	}
	pattern := args
	inputPat := ""
	if idx := strings.Index(pattern, "|"); idx >= 0 {
		inputPat = strings.TrimSpace(pattern[idx+1:])
		pattern = strings.TrimSpace(pattern[:idx])
	}
	if pattern == "" {
		return "usage: /allow <pattern> [| <inputPattern>] [--global]"
	}
	if err := permission.Allow(permCWD, pattern, inputPat, global); err != nil {
		return fmt.Sprintf("allow failed: %v", err)
	}
	scope := "project " + permission.ProjectPath(permCWD)
	if global {
		if gp, err := permission.GlobalPath(); err == nil {
			scope = "global " + gp
		}
	}
	if inputPat != "" {
		return fmt.Sprintf("persisted allow %q (input:%q) -> %s", pattern, inputPat, scope)
	}
	return fmt.Sprintf("persisted allow %q -> %s", pattern, scope)
}

func runDeny(args string) string {
	if args == "" {
		return "usage: /deny <pattern> [| <inputPattern>] [--global]"
	}
	global := false
	if strings.Contains(args, "--global") {
		global = true
		args = strings.ReplaceAll(args, "--global", "")
		args = strings.ReplaceAll(args, "-g", "")
		args = strings.TrimSpace(args)
	}
	pattern := args
	inputPat := ""
	if idx := strings.Index(pattern, "|"); idx >= 0 {
		inputPat = strings.TrimSpace(pattern[idx+1:])
		pattern = strings.TrimSpace(pattern[:idx])
	}
	if pattern == "" {
		return "usage: /deny <pattern> [| <inputPattern>] [--global]"
	}
	if err := permission.Deny(permCWD, pattern, inputPat, global); err != nil {
		return fmt.Sprintf("deny failed: %v", err)
	}
	scope := "project " + permission.ProjectPath(permCWD)
	if global {
		if gp, err := permission.GlobalPath(); err == nil {
			scope = "global " + gp
		}
	}
	if inputPat != "" {
		return fmt.Sprintf("persisted deny %q (input:%q) -> %s", pattern, inputPat, scope)
	}
	return fmt.Sprintf("persisted deny %q -> %s", pattern, scope)
}

func runRevoke(args string) string {
	if args == "" {
		return "usage: /revoke <pattern> [--global]"
	}
	global := false
	if strings.Contains(args, "--global") {
		global = true
		args = strings.ReplaceAll(args, "--global", "")
		args = strings.ReplaceAll(args, "-g", "")
		args = strings.TrimSpace(args)
	}
	pattern := strings.TrimSpace(args)
	if pattern == "" {
		return "usage: /revoke <pattern> [--global]"
	}
	if err := permission.Revoke(permCWD, pattern, global); err != nil {
		return fmt.Sprintf("revoke failed: %v", err)
	}
	return fmt.Sprintf("revoked %q", pattern)
}

func runPermList() string {
	p, err := permission.Load(permCWD)
	if err != nil {
		return fmt.Sprintf("list failed: %v", err)
	}
	if len(p.Allow) == 0 && len(p.Deny) == 0 {
		return "no persisted permissions (project + global merged)"
	}
	var b strings.Builder
	b.WriteString("persisted permissions (merged):\n")
	if len(p.Allow) > 0 {
		b.WriteString("  allow:\n")
		for _, r := range p.Allow {
			if r.InputPattern != "" {
				fmt.Fprintf(&b, "    - %s | %s", r.Pattern, r.InputPattern)
			} else {
				fmt.Fprintf(&b, "    - %s", r.Pattern)
			}
			if r.AddedAt != "" {
				fmt.Fprintf(&b, "  (%s)", r.AddedAt)
			}
			b.WriteString("\n")
		}
	}
	if len(p.Deny) > 0 {
		b.WriteString("  deny:\n")
		for _, r := range p.Deny {
			if r.InputPattern != "" {
				fmt.Fprintf(&b, "    - %s | %s", r.Pattern, r.InputPattern)
			} else {
				fmt.Fprintf(&b, "    - %s", r.Pattern)
			}
			if r.AddedAt != "" {
				fmt.Fprintf(&b, "  (%s)", r.AddedAt)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("project: " + permission.ProjectPath(permCWD) + "\n")
	if gp, err := permission.GlobalPath(); err == nil {
		b.WriteString("global: " + gp)
	}
	return b.String()
}
