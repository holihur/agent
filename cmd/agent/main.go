// Command agent 是最小 Agent 的装配入口。
//
// LLM 配置(env 或 .env,已有环境变量优先):
//
//	LLM_API_KEY(或 LLM_APIKEY)、LLM_BASE_URL(Anthropic 兼容端点)、LLM_MODEL
//	LLM_AUTH_STYLE: bearer(默认) | x-api-key | both
//	-provider NAME(或 LLM_PROVIDER=NAME)时优读 NAME_API_KEY/NAME_APIKEY、NAME_BASE_URL、NAME_MODEL
//
// 用法:
//
//	agent                                        # CLI REPL,仅内置工具
//	agent -mcp "fs=npx @modelcontextprotocol/server-filesystem /tmp"
//	agent -q "3+5 等于几" -mcp "gh=gh-mcp-server"  # 一次性执行
//	agent -agents-md off                         # 禁用 AGENTS.md 注入(auto 默认从 cwd 逐层向上发现)
//	agent -skills off                            # 禁用技能扫描(默认扫描 cwd 下 .agents/skills/)
//	agent -shell off                             # 禁用内置 shell 工具(模型不再能执行命令)
//	agent -fs off                                # 禁用内置文件工具 read/write/edit(模型不再能直接读写文件)
//	agent -shell-escape off                      # 禁用 REPL "!" shell 逃逸(仅用户手动触发,与 -shell 互不影响)
//	agent -slashcmd off                             # 禁用 REPL "/" 命令(/help 打印帮助文档,与 -shell 互不影响)
//	agent -pprof localhost:6060                  # 开启 pprof 诊断端点(on = localhost:6060;默认关闭)
//	agent -sessions                              # 列出已保存会话(cwd 下 .agent/sessions)
//	agent -session work                          # 续接会话 work(不存在则新建),每轮自动保存
//	agent -temperature 0.2                       # 采样温度(<0 = 端点默认)
//	agent -reasoning-effort high                 # 推理力度透传(空 = 端点默认)
//
// MCP 服务器来源(可叠加,规范 docs/mcp.json.spec.md):
//   - 文件:cwd 下 mcp.json(或 .mcp.json)的 mcpServers 对象(command=stdio / url=http)
//   - 标志:-mcp <name>=<command> [args...](可重复;同名覆盖文件条目)
//   - 标志远程:-mcp <name>=https://host/mcp(Streamable HTTP)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/llm"
	"github.com/holihur/agent/internal/mcp"
	"github.com/holihur/agent/internal/session"
	"github.com/holihur/agent/internal/tools"
	uicli "github.com/holihur/agent/internal/ui/cli"
	"github.com/holihur/agent/internal/utils"

	// 钩子功能包:各自在 init 中向 hook 注册(新增功能 = 新增子目录 + 此处一行)。
	_ "github.com/holihur/agent/internal/hook/agentsmd"
	_ "github.com/holihur/agent/internal/hook/confirm"
	_ "github.com/holihur/agent/internal/hook/pprof"
	_ "github.com/holihur/agent/internal/hook/shell"
	_ "github.com/holihur/agent/internal/hook/skills"
	_ "github.com/holihur/agent/internal/hook/slashcmd"
	_ "github.com/holihur/agent/internal/hook/termtitle"
	_ "github.com/holihur/agent/internal/hook/verbose"
)

// serverNameRe 与 tools 层命名空间校验保持一致(提前拦截,报错更友好)。
var serverNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const defaultSystem = "You are a helpful assistant. Use the available tools when they help answer the question."

// mcpFlags 支持可重复的 -mcp <name>=<command> [args...]。
type mcpFlags []mcpServer

type mcpServer struct {
	Name    string
	Command string
	Args    []string
	URL     string // 以 http(s):// 开头时为远程服务器(与 Command 互斥)
}

func (f *mcpFlags) String() string {
	if f == nil {
		return ""
	}
	parts := make([]string, 0, len(*f))
	for _, s := range *f {
		parts = append(parts, s.Name+"="+s.Command+" "+strings.Join(s.Args, " "))
	}
	return strings.Join(parts, ", ")
}

func (f *mcpFlags) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || !serverNameRe.MatchString(name) {
		return fmt.Errorf("-mcp must be <name>=<command> [args...] or <name>=<http(s)://url>, got %q", v)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("-mcp %s: missing command", name)
	}
	if isHTTPURL(fields[0]) {
		if len(fields) > 1 {
			return fmt.Errorf("-mcp %s: remote url takes no args", name)
		}
		*f = append(*f, mcpServer{Name: name, URL: fields[0]})
		return nil
	}
	*f = append(*f, mcpServer{Name: name, Command: fields[0], Args: fields[1:]})
	return nil
}

// isHTTPURL 判断 flag 值是否指向远程服务器(规范 §mcp.json/remote)。
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// serverSpec 是装配一个 MCP Provider 所需的最终配置。
// URL 非空 → Streamable HTTP;否则 Command + Args → stdio 子进程。
type serverSpec struct {
	Name    string
	Command string
	Args    []string
	Env     []string
	URL     string
	Headers map[string]string
}

// mergeMCPServers 合并 mcp.json 条目与 -mcp flag(规范 §mcp.json/merge):
// 文件条目按文档顺序在前,flag 按顺序追加;同名时 flag 覆盖文件条目。
// env 转为键名排序的 "K=V" 切片,追加到子进程继承环境之后。
func mergeMCPServers(fromFile []mcp.JSONServer, fromFlags mcpFlags) []serverSpec {
	specs := make([]serverSpec, 0, len(fromFile)+len(fromFlags))
	index := make(map[string]int, len(fromFile)+len(fromFlags))
	add := func(s serverSpec) {
		if i, ok := index[s.Name]; ok {
			specs[i] = s
			return
		}
		index[s.Name] = len(specs)
		specs = append(specs, s)
	}
	for _, s := range fromFile {
		add(serverSpec{
			Name: s.Name, Command: s.Command, Args: s.Args,
			Env: envPairs(s.Env), URL: s.URL, Headers: s.Headers,
		})
	}
	for _, s := range fromFlags {
		add(serverSpec{Name: s.Name, Command: s.Command, Args: s.Args, URL: s.URL})
	}
	return specs
}

// envPairs 把 env 映射转为键名排序的 "K=V" 切片(排序保证确定性)。
func envPairs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// buildMCPProviders 按 spec 装配 Provider 并做启动预检(ListTools 即建连 + 探测 + 缓存)。
// 失败分流(规范 §mcp.json/remote):远程服务器不可达/协议不符 → 向 warnW 警告并跳过,
// 进程继续;stdio 失败(命令不存在等配置错误)→ 返回 error,fail-fast。
func buildMCPProviders(ctx context.Context, specs []serverSpec, warnW io.Writer, responder tools.Responder) ([]*mcp.Provider, error) {
	var providers []*mcp.Provider
	for _, s := range specs {
		var p *mcp.Provider
		if s.URL != "" {
			p = mcp.NewHTTP(s.Name, mcp.HTTPConfig{URL: s.URL, Headers: s.Headers}, responder)
		} else {
			p = mcp.NewStdio(s.Name, mcp.StdioConfig{Command: s.Command, Args: s.Args, Env: s.Env}, responder)
		}
		if _, err := p.ListTools(ctx); err != nil {
			_ = p.Close()
			if s.URL != "" {
				fmt.Fprintf(warnW, "mcp: skipping remote server %q: %v\n", s.Name, err)
				continue
			}
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		servers  mcpFlags
		quick    = flag.String("q", "", "one-shot question (default: interactive REPL)")
		system   = flag.String("system", defaultSystem, "system prompt")
		provider = flag.String("provider", "", "env prefix: NAME reads NAME_API_KEY/NAME_APIKEY, NAME_BASE_URL, NAME_MODEL")
		model    = flag.String("model", "", "model override (default: LLM_MODEL or NAME_MODEL)")
		maxToks  = flag.Int("max-tokens", 1024, "max_tokens per LLM turn")
		maxTurns = flag.Int("max-turns", 60, "max think-act-observe turns per question (<=0 = default 60)")
		temp     = flag.Float64("temperature", -1, "sampling temperature; <0 = endpoint default (main)")
		effort   = flag.String("reasoning-effort", "", "reasoning effort passed through, e.g. low/medium/high; empty = endpoint default (main)")
		shell    = flag.String("shell", "on", "builtin shell tool; off/none disables (main)")
		fs       = flag.String("fs", "on", "builtin file tools read/write/edit; off/none disables (main)")
		sessName = flag.String("session", "", "persistent session name: resume if exists, autosave each turn (main)")
		sessList = flag.Bool("sessions", false, "list saved sessions and exit (main)")
	)
	flag.Var(&servers, "mcp", "MCP stdio server, repeatable: <name>=<command> [args...]")
	flag.Parse()

	switch *shell {
	case "", "on", "off", "none":
	default:
		return fmt.Errorf("-shell must be on or off/none, got %q", *shell)
	}
	switch *fs {
	case "", "on", "off", "none":
	default:
		return fmt.Errorf("-fs must be on or off/none, got %q", *fs)
	}

	utils.LoadDotEnv(".env")

	// 会话存储:-sessions 是纯存储操作,置于凭据校验之前(无凭据也可列出)。
	store := session.NewFileStore(".agent/sessions")
	if *sessList {
		names, err := store.Names(context.Background())
		if err != nil {
			return err
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	}

	prefix := ""
	if p := strings.TrimSpace(*provider); p != "" {
		prefix = strings.ToUpper(p) + "_"
	} else if p := os.Getenv("LLM_PROVIDER"); p != "" {
		prefix = strings.ToUpper(p) + "_"
	}
	keyNames := []string{"LLM_API_KEY", "LLM_APIKEY"}
	baseNames := []string{"LLM_BASE_URL"}
	modelNames := []string{"LLM_MODEL"}
	if prefix != "" {
		keyNames = append([]string{prefix + "API_KEY", prefix + "APIKEY"}, keyNames...)
		baseNames = append([]string{prefix + "BASE_URL"}, baseNames...)
		modelNames = append([]string{prefix + "MODEL"}, modelNames...)
	}

	apiKey := envFirst(keyNames...)
	baseURL := envFirst(baseNames...)
	llmModel := envFirst(modelNames...)
	authStyle := envFirst(prefix+"AUTH_STYLE", "LLM_AUTH_STYLE")
	if *model != "" {
		llmModel = *model
	}
	if apiKey == "" {
		return fmt.Errorf("no API key: set LLM_API_KEY (or NAME_API_KEY/NAME_APIKEY with -provider) in env or .env")
	}
	if baseURL == "" {
		return fmt.Errorf("no base URL: set LLM_BASE_URL (an Anthropic-compatible endpoint)")
	}
	if llmModel == "" {
		return fmt.Errorf("no model: set LLM_MODEL (or NAME_MODEL with -provider)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	registry := tools.New()
	// builtin 同时充当 hook 的进程内工具平面(如 skills 注册 skill 工具),
	// -shell/-fs off 时保留空平面,只不注册对应工具。
	builtin := tools.NewLocal()
	if *shell != "off" && *shell != "none" {
		if err := tools.RegisterShell(builtin); err != nil {
			return err
		}
	}
	if *fs != "off" && *fs != "none" {
		if err := tools.RegisterFS(builtin); err != nil {
			return err
		}
	}
	if err := registry.Register(builtin); err != nil {
		return err
	}

	ui := uicli.New(os.Stdin, os.Stdout)

	// 生命周期钩子:每个 hook 是 internal/hook/ 下一个子包,init 自注册,
	// 上方 blank-import 激活;这里只统一装配 InstallAll。
	// builtin 同时充当 hook 的进程内工具平面(如 skills 注册 skill 工具)。
	hooks := agent.NewHooks()
	if err := hook.InstallAll(hooks, hook.Deps{CWD: cwd, UI: ui, Tools: builtin}); err != nil {
		return err
	}

	// MCP 服务器:mcp.json 文件条目与 -mcp flag 合并(规范 §mcp.json/merge),
	// 每个是一个 Provider;REPL UI 同时充当 MRTR 应答者。
	fromFile, err := mcp.LoadJSONConfig(
		filepath.Join(cwd, "mcp.json"),
		filepath.Join(cwd, ".mcp.json"),
	)
	if err != nil {
		return err
	}
	// MCP 服务器:mcp.json 文件条目与 -mcp flag 合并(规范 §mcp.json/merge)。
	// 启动预检分流(规范 §mcp.json/remote):远程失败 → 警告并跳过;stdio 失败 → fail-fast。
	mcpProviders, err := buildMCPProviders(ctx, mergeMCPServers(fromFile, servers), os.Stderr, ui)
	if err != nil {
		return err
	}
	defer func() {
		for _, p := range mcpProviders {
			_ = p.Close()
		}
	}()
	for _, p := range mcpProviders {
		if err := registry.Register(p); err != nil {
			return err
		}
	}

	// 预检:聚合工具列表(暴露名冲突 fail-fast;连接已由 buildMCPProviders 分流处理)。
	defs, err := registry.Tools(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	fmt.Fprintf(os.Stderr, "tools: %s\n", strings.Join(names, ", "))

	llmClient := llm.New(apiKey, baseURL, llmModel, *maxToks)
	llmClient.AuthStyle = authStyle
	if *temp >= 0 {
		if *temp > 1 {
			return fmt.Errorf("-temperature must be in [0,1], got %v", *temp)
		}
		llmClient.Temperature = temp
	}
	llmClient.ReasoningEffort = *effort
	ag := &agent.Agent{LLM: llmClient, Registry: registry, System: *system, Hooks: hooks, MaxTurns: *maxTurns}

	ui.Agent = ag                       // 两阶段装配:Responder(即 UI)先于 Agent 可用
	ui.Model = llmClient.Model          // banner 显示最终解析的模型(env/flag/provider 归一后)
	ag.OnTextDelta = ui.TextDeltaSink() // 流式增量 → 终端

	// 会话持久化:-session 指定时,启动续接(不存在则新建),每轮 Run 后自动保存。
	// /new 轮转:NewSession 与 AfterRun 共享 active 变量,轮转后自动保存落新文件,
	// 旧会话文件原样保留;轮转失败保持原状(fail-loud 提示,不清历史)。
	if *sessName != "" {
		msgs, err := store.Load(ctx, *sessName)
		switch {
		case err == nil:
			ag.Messages = msgs
			fmt.Fprintf(os.Stderr, "session: resumed %s (%d messages)\n", *sessName, len(msgs))
		case errors.Is(err, agent.ErrSessionNotFound):
			fmt.Fprintf(os.Stderr, "session: new %s\n", *sessName)
		default:
			return err
		}
		active := *sessName
		ui.AfterRun = func(runErr error) {
			if err := store.Save(ctx, active, ag.Messages); err != nil {
				fmt.Fprintf(os.Stderr, "session: save: %v\n", err)
			}
		}
		ui.NewSession = func() string {
			next, err := session.NextName(ctx, store, active)
			if err != nil {
				return fmt.Sprintf("session: rotate failed: %v (history kept)", err)
			}
			kept := active
			active = next
			ag.Messages = nil
			return fmt.Sprintf("session: new %s (kept %s)", next, kept)
		}
	}

	if *quick != "" {
		return ui.RunOnce(ctx, *quick)
	}
	return ui.Run(ctx)
}

func envFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}
