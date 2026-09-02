// Package slashcmd 实现 REPL "/" 命令:以 "/" 开头的输入按命令处理,
// 命令结果作为本轮回答返回,不进对话历史、不调模型。默认开启,
// -slashcmd off/none 可禁用。"/" 前缀与 shell 逃逸 "!" 前缀一样是
// REPL 层面的保留输入形态,正常提问(不以 / 开头)不受影响。
package slashcmd

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/permission"
	"github.com/holihur/agent/internal/session"
)

var slashCmd = flag.String("slashcmd", "on", `REPL "/" commands, e.g. /help; off/none disables (hook)`)

// permCWD 保存当前项目根，供斜杠命令读写权限文件。
var permCWD string

// compact 上下文，供 /compact 手动压缩使用
var (
	compactStore    *session.FileStore
	compactActive   *string
	compactMessages *[]agent.Message
	compactCfg      *session.Config
)

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

// SetCompactContext 供 cmd 注入会话压缩上下文，使 /compact 可操作当前会话
func SetCompactContext(store *session.FileStore, active *string, msgs *[]agent.Message, cfg *session.Config) {
	compactStore = store
	compactActive = active
	compactMessages = msgs
	compactCfg = cfg
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
  /compact [session] [--keep N]  manually compress session (force, keep last N, default 6) -> s_xid_1
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
	if trim == "/compact" || strings.HasPrefix(trim, "/compact ") || trim == "/compact help" {
		if trim == "/compact help" {
			return compactHelpText, true
		}
		if strings.HasPrefix(trim, "/compact ") {
			args := strings.TrimSpace(strings.TrimPrefix(trim, "/compact"))
			if args == "help" {
				return compactHelpText, true
			}
			return runCompact(args), true
		}
		return runCompact(""), true
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

const compactHelpText = `Compact commands:
  /compact [session] [--keep N]  force compress session (default current, keep last N, default 6) -> s_xid_1
    session 不指定则压缩当前 -session，指定则压缩该命名会话
    --keep N 指定压缩后保留的最近消息数
  示例: /compact  /compact s_a1b2c3d4 --keep 10`

func runCompact(args string) string {
	if compactStore == nil || compactActive == nil || compactMessages == nil || compactCfg == nil {
		return "compact unavailable: no active session (use -session)"
	}
	// 解析 --keep
	keep := compactCfg.KeepRecent
	target := ""
	tokens := strings.Fields(args)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "--keep" && i+1 < len(tokens) {
			if v, err := strconv.Atoi(tokens[i+1]); err == nil {
				keep = v
			}
			i++
			continue
		}
		if strings.HasPrefix(tok, "--keep=") {
			if v, err := strconv.Atoi(strings.TrimPrefix(tok, "--keep=")); err == nil {
				keep = v
			}
			continue
		}
		if target == "" && !strings.HasPrefix(tok, "--") {
			target = tok
		}
	}
	// 确定待压缩的会话与消息
	var msgs []agent.Message
	var base string
	if target != "" {
		// 压缩指定会话
		m, err := compactStore.Load(context.Background(), target)
		if err != nil {
			return fmt.Sprintf("compact failed: load %q: %v", target, err)
		}
		msgs = m
		base = target
	} else {
		if *compactActive == "" {
			return "compact failed: no active session"
		}
		// 优先用内存消息，确保包含未落盘的最新轮次
		if compactMessages != nil && len(*compactMessages) > 0 {
			msgs = *compactMessages
		} else {
			m, err := compactStore.Load(context.Background(), *compactActive)
			if err != nil {
				return fmt.Sprintf("compact failed: load %q: %v", *compactActive, err)
			}
			msgs = m
		}
		base = *compactActive
	}
	if len(msgs) == 0 {
		return "compact: no messages to compress"
	}
	cfg := *compactCfg
	if keep > 0 {
		cfg.KeepRecent = keep
	}
	// 手动强制压缩：即使 keep >= len 也要产生摘要，避免“复制”
	if len(msgs) <= cfg.KeepRecent {
		if len(msgs) > 1 {
			cfg.KeepRecent = len(msgs) - 1
			if cfg.KeepRecent < 1 {
				cfg.KeepRecent = 1
			}
		} else {
			return "compact: only 1 message, nothing to compress"
		}
	}
	newMsgs, err := session.CompressMessages(context.Background(), msgs, cfg)
	if err != nil {
		return fmt.Sprintf("compact failed: %v", err)
	}
	names, _ := compactStore.Names(context.Background())
	newName := session.NextCompressedName(base, names)
	if err := compactStore.Save(context.Background(), newName, newMsgs); err != nil {
		return fmt.Sprintf("compact failed: save %q: %v", newName, err)
	}
	// 若压缩的是当前会话，切换 active
	if target == "" || target == *compactActive {
		*compactActive = newName
		*compactMessages = newMsgs
		return fmt.Sprintf("compacted %s -> %s (kept %d, %d -> %d)", base, newName, cfg.KeepRecent, len(msgs), len(newMsgs))
	}
	return fmt.Sprintf("compacted %s -> %s (kept %d, %d -> %d)", base, newName, cfg.KeepRecent, len(msgs), len(newMsgs))
}
