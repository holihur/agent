// Command agent 是最小 Agent 的装配入口。
//
// LLM 配置(env 或 .env,已有环境变量优先):
//
//	LLM_API_KEY(或 LLM_APIKEY)、LLM_BASE_URL、LLM_MODEL
//	LLM_API: anthropic(默认) | openai | responses
//	  anthropic → POST {base}/v1/messages
//	  openai    → POST {base}/v1/chat/completions(Chat Completions 老接口)
//	  responses → POST {base}/v1/responses(Responses API 新接口)
//	LLM_AUTH_STYLE: bearer(默认) | x-api-key | both(仅 anthropic)
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
//	agent -addr 0.0.0.0:8788                     # 作为 MCP Streamable HTTP 服务器常驻(供其他 agent 调用)
//	agent -addr 0.0.0.0:8788 -mdns -name work    # 同上,并经 mDNS 广播(_agent-mcp._tcp)供局域网发现
//	agent -sessions                              # 列出已保存会话(cwd 下 .agent/sessions)
//	agent -version                               # 打印版本号后退出
//	agent -update                                # 自我升级:下载最新 release 替换当前二进制
//	agent -session work                          # 续接会话 work(不存在则新建),每轮自动保存
//	agent -temperature 0.2                       # 采样温度(<0 = 端点默认)
//	agent -reasoning-effort high                 # 推理力度透传(空 = 端点默认)
//	agent -api responses                         # 协议风格:anthropic|openai|responses(空 = LLM_API 或 anthropic)
//	agent init                                   # 交互式生成 cwd 下 .env(已存在则拒绝)
//	agent -q                                     # 无参数 + 管道 stdin 时读入整段输入当一次提问(echo hi | agent)
//
// agent.json(cwd 下,可选):provider/api/model/max_tokens/max_turns/temperature/
// reasoning_effort/session/session_compress/compress_* 等字段作为 flag 默认值,flag 始终可覆盖。
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
	"time"

	_ "embed"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/apisrv"
	"github.com/holihur/agent/internal/daemon"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/hook/slashcmd"
	"github.com/holihur/agent/internal/llm"
	"github.com/holihur/agent/internal/mcp"
	"github.com/holihur/agent/internal/mcpserver"
	"github.com/holihur/agent/internal/mdns"
	"github.com/holihur/agent/internal/selfupdate"
	"github.com/holihur/agent/internal/session"
	"github.com/holihur/agent/internal/tools"
	uicli "github.com/holihur/agent/internal/ui/cli"
	"github.com/holihur/agent/internal/utils"

	"golang.org/x/term"

	// 钩子功能包:各自在 init 中向 hook 注册(新增功能 = 新增子目录 + 此处一行)。
	_ "github.com/holihur/agent/internal/hook/agentsmd"
	_ "github.com/holihur/agent/internal/hook/confirm"
	_ "github.com/holihur/agent/internal/hook/memory"
	_ "github.com/holihur/agent/internal/hook/perm"
	_ "github.com/holihur/agent/internal/hook/pprof"
	_ "github.com/holihur/agent/internal/hook/shell"
	_ "github.com/holihur/agent/internal/hook/skills"
	_ "github.com/holihur/agent/internal/hook/slashcmd"
	_ "github.com/holihur/agent/internal/hook/termtitle"
	_ "github.com/holihur/agent/internal/hook/verbose"
)

// serverNameRe 与 tools 层命名空间校验保持一致(提前拦截,报错更友好)。
var serverNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// version 由 GoReleaser ldflags 注入(见 .goreleaser.yml);本地构建默认 "dev"。
var version = "dev"

// defaultSystem 是嵌入式默认系统提示词:随二进制编译进程序(见 system_prompt.md),
// 并追加 gen-usage 从 `agent -h` 生成的 CLI 用法(见 usage.md,经 //go:generate 同步),
// 无需随可执行文件分发额外资源;可用 -system 覆盖。
//
//go:generate go run ../gen-usage -out usage.md
//go:embed system_prompt.md
var baseSystem string

//go:embed usage.md
var usageDoc string

var defaultSystem = baseSystem + "\n" + usageDoc

// mcpFlags 支持可重复的 -mcp <name>=<command> [args...]。
type mcpFlags []mcpServer

type mcpServer struct {
	Name        string
	Command     string
	Args        []string
	URL         string // 以 http(s):// 开头时为远程服务器(与 Command 互斥)
	ServiceName string // 以 mdns: 开头时为 mDNS 发现的实例名(空 = 首个)(与 Command 互斥)
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
	if inst, ok := strings.CutPrefix(fields[0], "mdns:"); ok {
		if len(fields) > 1 {
			return fmt.Errorf("-mcp %s: mdns: url takes no args", name)
		}
		*f = append(*f, mcpServer{Name: name, ServiceName: inst})
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
	Name        string
	Command     string
	Args        []string
	Env         []string
	URL         string
	Headers     map[string]string
	ServiceName string // mDNS 发现的实例名(空 = 首个);解析后填入 URL
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
		add(serverSpec{Name: s.Name, Command: s.Command, Args: s.Args, URL: s.URL, ServiceName: s.ServiceName})
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
		if s.URL == "" && s.ServiceName != "" {
			// mDNS 发现:实例名解析为 http URL 后按远程 HTTP 挂载;
			// 发现失败按远程语义警告并跳过。
			url, err := mdns.ResolveURL(ctx, s.ServiceName, 3*time.Second)
			if err != nil {
				fmt.Fprintf(warnW, "mcp: skipping remote server %q: %v\n", s.Name, err)
				continue
			}
			s.URL = url
		}
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

// 退出码:0 成功;1 运行/未知错误;2 用法错误(flag、agent.json、参数校验);
// 3 凭据/配置缺失;4 MCP/上游连接失败。调用方(脚本、CI)可据此分类处理。
const (
	exitOK      = 0
	exitGeneric = 1
	exitUsage   = 2
	exitConfig  = 3
	exitConn    = 4
)

// exitError 携带进程退出码的错误;main 据此设置 os.Exit 码。
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// failf 构造带退出码的错误(fmt.Errorf 语义)。
func failf(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// fail 包装已有错误并赋予退出码(err 为 nil 时返回 nil,便于直接 return fail(...)。
// 注意:此处仅用于非 nil 错误,见各调用点)。
func fail(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

func main() {
	if err := run(); err != nil {
		code := exitGeneric
		var ee *exitError
		if errors.As(err, &ee) {
			code = ee.code
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(code)
	}
}

func run() error {
	// workspace:-C 先切换工作目录,后续一切(session/.env/mcp.json/skills/
	// agent.json/daemon pidfile 与 log)都以该目录为根,实现按 workspace 隔离。
	ws, err := parseWorkspaceFlag(os.Args[1:])
	if err != nil {
		return fail(exitUsage, err)
	}
	if ws != "" {
		abs, err := filepath.Abs(ws)
		if err != nil {
			return fail(exitUsage, err)
		}
		ws = abs
		info, err := os.Stat(ws)
		switch {
		case err != nil:
			return fail(exitUsage, fmt.Errorf("-C %s: %w", ws, err))
		case !info.IsDir():
			return fail(exitUsage, fmt.Errorf("-C %s: not a directory", ws))
		}
		if err := os.Chdir(ws); err != nil {
			return fail(exitUsage, err)
		}
	}

	// agent.json:cwd 下配置文件,值作为 flag 默认值(flag 始终可覆盖);
	// 放在 flag 定义前,使默认值即配置值。
	cfg, err := loadAppConfig("agent.json")
	if err != nil {
		return fail(exitUsage, err)
	}

	var (
		servers       mcpFlags
		quick         = flag.String("q", "", "one-shot question (default: interactive REPL; with piped stdin, reads the question from stdin)")
		system        = flag.String("system", defaultSystem, "system prompt")
		provider      = flag.String("provider", stringOr(cfg.Provider, ""), "env prefix: NAME reads NAME_API_KEY/NAME_APIKEY, NAME_BASE_URL, NAME_MODEL")
		model         = flag.String("model", stringOr(cfg.Model, ""), "model override (default: LLM_MODEL or NAME_MODEL)")
		maxToks       = flag.Int("max-tokens", intOr(cfg.MaxTokens, 1024), "max_tokens per LLM turn")
		maxTurns      = flag.Int("max-turns", intOr(cfg.MaxTurns, 60), "max think-act-observe turns per question (<=0 = default 60)")
		temp          = flag.Float64("temperature", floatOr(cfg.Temperature, -1), "sampling temperature; <0 = endpoint default (main)")
		effort        = flag.String("reasoning-effort", stringOr(cfg.ReasoningEffort, ""), "reasoning effort passed through, e.g. low/medium/high; empty = endpoint default (main)")
		api           = flag.String("api", stringOr(cfg.API, ""), "LLM API style: anthropic|openai|responses; empty = LLM_API env or anthropic (main)")
		shell         = flag.String("shell", "on", "builtin shell tool; off/none disables (main)")
		fs            = flag.String("fs", "on", "builtin file tools read/write/edit; off/none disables (main)")
		sessName      = flag.String("session", stringOr(cfg.Session, ""), "persistent session name: resume if exists, autosave each turn (main)")
		sessList      = flag.Bool("sessions", false, "list saved sessions and exit (main)")
		compressMode  = flag.String("session-compress", stringOr(cfg.SessionCompress, "auto"), "session compress: auto/on/off (auto triggers at threshold)")
		compressMax   = flag.Int("compress-max-tokens", intOr(cfg.CompressMaxToks, 100000), "session compress max tokens (e.g. 100000)")
		compressRatio = flag.Float64("compress-ratio", floatOr(cfg.CompressRatio, 0.8), "session compress trigger ratio 0-1 (e.g. 0.8 means 80%)")
		compressKeep  = flag.Int("compress-keep", intOr(cfg.CompressKeep, 6), "recent messages to keep after compress")
		showVersion   = flag.Bool("version", false, "print version and exit (main)")
		doUpdate      = flag.Bool("update", false, "self-update: download latest release and replace this binary (main)")
		addr          = flag.String("addr", "", "serve MCP Streamable HTTP on this address (e.g. 0.0.0.0:8788) instead of REPL; tools: agent_run(text, session?), agent_sessions()")
		apiAddr       = flag.String("api-addr", "", "serve the chat HTTP API + web UI on this address (e.g. 127.0.0.1:8790) instead of the REPL; enables shell/fs tools")
		mdnsOn        = flag.Bool("mdns", false, "announce this agent via mDNS (_agent-mcp._tcp) for discovery (requires -addr or -daemon)")
		mdnsName      = flag.String("name", "agent-mcp", "mDNS instance name (requires -mdns)")
		callTimeout   = flag.Duration("timeout", 10*time.Minute, "per-call timeout for -addr/-daemon server mode")
		daemonStart   = flag.Bool("daemon", false, "start the MCP server (-addr, default 127.0.0.1:8788) in the background and exit; logs to .agent/agent-mcp.log")
		daemonStop    = flag.Bool("daemon-stop", false, "stop the background daemon started with -daemon")
		_             = flag.String("C", "", "workspace root: run everything under this directory (applied before flag defaults; see pre-scan)")
	)
	flag.Var(&servers, "mcp", "MCP stdio server, repeatable: <name>=<command> [args...]")
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		return fail(exitUsage, err)
	}

	if flag.Arg(0) == "init" {
		return runInit(".", os.Stdin, os.Stdout)
	}

	if *showVersion {
		fmt.Printf("agent %s\n", version)
		return nil
	}
	if *doUpdate {
		res, err := selfupdate.Run(context.Background(), selfupdate.Options{
			Version:  version,
			Progress: func(msg string) { fmt.Fprintln(os.Stdout, msg) },
		})
		if err != nil {
			return err
		}
		if !res.Updated {
			return nil
		}
		fmt.Println("re-run `agent -version` to confirm")
		return nil
	}

	switch *shell {
	case "", "on", "off", "none":
	default:
		return failf(exitUsage, "-shell must be on or off/none, got %q", *shell)
	}
	switch *fs {
	case "", "on", "off", "none":
	default:
		return failf(exitUsage, "-fs must be on or off/none, got %q", *fs)
	}

	utils.LoadDotEnv(".env")

	// 守护进程:-daemon 后台启动 MCP 服务器(分离进程 + pidfile);-daemon-stop 结束之。
	// .env 已加载但不会传给后台进程(其启动后自行读取 cwd 下的 .env)。
	if *daemonStop {
		stopped, err := daemon.Stop()
		if err != nil {
			return err
		}
		if !stopped {
			fmt.Println("daemon: not running")
			return nil
		}
		fmt.Println("daemon: stopped")
		return nil
	}
	if *daemonStart {
		srvAddr := *addr
		if srvAddr == "" {
			srvAddr = "127.0.0.1:8788"
		}
		args := []string{"-addr", srvAddr, "-timeout", callTimeout.String()}
		if ws != "" {
			args = append(args, "-C", ws)
		}
		if *mdnsOn {
			args = append(args, "-mdns", "-name", *mdnsName)
		}
		started, err := daemon.Start(args...)
		if err != nil {
			return err
		}
		if !started {
			fmt.Println("daemon: already running")
			return nil
		}
		fmt.Printf("daemon: started (addr %s, pid in %s, log at %s)\n", srvAddr, daemon.PidFile(), daemon.LogFile())
		return nil
	}

	// 服务器模式:-addr 时把 agent 暴露为 MCP Streamable HTTP 服务器常驻,
	// Agent 实例按 session 惰性构造(凭据走 env/.env),故置于凭据校验之前。
	if *addr != "" {
		name := ""
		if *mdnsOn {
			name = *mdnsName
		}
		return mcpserver.Run(*addr, *callTimeout, name)
	}

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
	// API 风格:flag(-api)> env(LLM_API)> 默认 anthropic。openai=Chat Completions 老接口,
	// responses=Responses API 新接口;具体适配器由 llm.NewAdapter 构建并校验取值。
	apiStyle := *api
	if apiStyle == "" {
		apiStyle = os.Getenv("LLM_API")
	}
	if apiStyle == "" {
		apiStyle = "anthropic"
	}
	if *model != "" {
		llmModel = *model
	}
	if apiKey == "" {
		return failf(exitConfig, "no API key: set LLM_API_KEY in env or .env (with -provider NAME set NAME_API_KEY instead)\n"+
			"  quick start: run `agent init` to create .env interactively, or:\n"+
			"  export LLM_API_KEY=sk-... LLM_BASE_URL=https://your-endpoint LLM_MODEL=your-model")
	}
	if baseURL == "" {
		return failf(exitConfig, "no base URL: set LLM_BASE_URL (an Anthropic-compatible endpoint), e.g. export LLM_BASE_URL=https://api.example.com")
	}
	if llmModel == "" {
		return failf(exitConfig, "no model: set LLM_MODEL (or NAME_MODEL with -provider), e.g. export LLM_MODEL=claude-sonnet-4-5")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 聊天 API/UI 模式：常驻 HTTP 服务（开启 shell/fs，面向编码 copilot）。
	// 凭据已在上方解析/校验，故置于此处。
	if *apiAddr != "" {
		var tempPtr *float64
		if *temp >= 0 {
			tempPtr = temp
		}
		return apisrv.Run(apisrv.Options{
			Addr:            *apiAddr,
			System:          *system,
			APIKey:          apiKey,
			BaseURL:         baseURL,
			Model:           llmModel,
			AuthStyle:       authStyle,
			MaxTokens:       *maxToks,
			MaxTurns:        *maxTurns,
			Temperature:     tempPtr,
			ReasoningEffort: *effort,
		})
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	registry := tools.New()
	// builtin 同时充当 hook 的进程内工具平面(如 skills 注册 skill 工具),
	// -shell/-fs off 时保留空平面,只不注册对应工具。
	builtin := tools.NewLocal()
	// exit 工具:LLM 可主动请求优雅退出(置位后 REPL 当前轮收尾即结束,退出码 0)。
	exitFlag := &tools.ExitFlag{}
	if err := tools.RegisterExit(builtin, exitFlag); err != nil {
		return err
	}
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
	ui.CWD = cwd
	ui.ExitRequested = exitFlag.Requested

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
		return fail(exitUsage, err)
	}
	// MCP 服务器:mcp.json 文件条目与 -mcp flag 合并(规范 §mcp.json/merge)。
	// 启动预检分流(规范 §mcp.json/remote):远程失败 → 警告并跳过;stdio 失败 → fail-fast。
	mcpProviders, err := buildMCPProviders(ctx, mergeMCPServers(fromFile, servers), os.Stderr, ui)
	if err != nil {
		return fail(exitConn, err)
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

	var tempPtr *float64
	if *temp >= 0 {
		if *temp > 1 {
			return fmt.Errorf("-temperature must be in [0,1], got %v", *temp)
		}
		tempPtr = temp
	}
	llmClient, err := llm.NewAdapter(apiStyle, llm.Config{
		APIKey:          apiKey,
		BaseURL:         baseURL,
		Model:           llmModel,
		MaxTokens:       *maxToks,
		AuthStyle:       authStyle,
		Temperature:     tempPtr,
		ReasoningEffort: *effort,
	})
	if err != nil {
		return fail(exitUsage, err)
	}
	ag := &agent.Agent{LLM: llmClient, Registry: registry, System: *system, Hooks: hooks, MaxTurns: *maxTurns}

	ui.Agent = ag                       // 两阶段装配:Responder(即 UI)先于 Agent 可用
	ui.Model = llmModel                 // banner 显示最终解析的模型(env/flag/provider 归一后)
	ag.OnTextDelta = ui.TextDeltaSink() // 流式增量 → 终端

	// 会话压缩配置：接口化，LLM 压缩为唯一实现
	compressor := session.LLMCompressor{
		Summarize: func(ctx context.Context, prompt string) (string, error) {
			req := agent.TurnRequest{
				System: "You are a helpful assistant that summarizes conversation history concisely, preserving key decisions and tool results.",
				Messages: []agent.Message{
					{Role: agent.RoleUser, Blocks: []agent.Block{{Type: agent.BlockText, Text: prompt}}},
				},
			}
			res, err := llmClient.Turn(ctx, req)
			if err != nil {
				return "", err
			}
			return res.Assistant.TextContent(), nil
		},
	}
	compressCfg := session.Config{
		MaxTokens:  *compressMax,
		Ratio:      *compressRatio,
		KeepRecent: *compressKeep,
		Compressor: compressor,
	}
	compressEnabled := *compressMode != "off" && *compressMode != "none"
	if compressEnabled {
		if err := compressCfg.Validate(); err != nil {
			return fail(exitUsage, fmt.Errorf("compress config: %w", err))
		}
	}

	// 会话持久化:-session 指定时,启动续接(不存在则新建),每轮 Run 后自动保存。
	// /new 轮转:NewSession 与 AfterRun 共享 active 变量,轮转后自动保存落新文件,
	// 旧会话文件原样保留;轮转失败保持原状(fail-loud 提示,不清历史)。
	// 压缩：达到阈值（默认 100w*80%）时自动压缩为新会话 dev_1，保留最近 N 条，接口化可切换 LLM 压缩
	if *sessName != "" {
		msgs, err := store.Load(ctx, *sessName)
		switch {
		case err == nil:
			ag.Messages = msgs
			fmt.Fprintf(os.Stderr, "session: resumed %s (%d messages, ~%d tokens)\n", *sessName, len(msgs), session.EstimateTokens(msgs))
		case errors.Is(err, agent.ErrSessionNotFound):
			fmt.Fprintf(os.Stderr, "session: new %s\n", *sessName)
		default:
			return err
		}
		active := *sessName
		// 供 /compact 手动压缩使用
		slashcmd.SetCompactContext(store, &active, &ag.Messages, &compressCfg)
		ui.AfterRun = func(runErr error) {
			if err := store.Save(ctx, active, ag.Messages); err != nil {
				fmt.Fprintf(os.Stderr, "session: save: %v\n", err)
				return
			}
			if !compressEnabled {
				return
			}
			if !session.ShouldCompress(ag.Messages, compressCfg) {
				return
			}
			newName, newMsgs, ok, err := session.MaybeCompress(ctx, store, active, ag.Messages, compressCfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "session compress failed: %v\n", err)
				return
			}
			if ok {
				fmt.Fprintf(os.Stderr, "session compressed: %s -> %s (kept %d recent, total %d -> %d)\n", active, newName, compressCfg.KeepRecent, len(ag.Messages), len(newMsgs))
				active = newName
				ag.Messages = newMsgs
			}
		}
		ui.NewSession = func() string {
			// 全新会话一律 s_xid，压缩产生的 s_xid_n 由 MaybeCompress 负责
			var next string
			for {
				cand := session.GenerateSID()
				if _, err := store.Load(ctx, cand); err != nil {
					next = cand
					break
				}
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
	// 管道输入:非终端 stdin(如 echo hi | agent)把 stdin 全文当一次提问,
	// 空输入则显式报错,避免静默落到 REPL。
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		piped, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		question := strings.TrimSpace(string(piped))
		if question == "" {
			return fmt.Errorf("empty stdin: pipe a question, e.g. echo 'summarize this repo' | agent")
		}
		return ui.RunOnce(ctx, question)
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

// parseWorkspaceFlag 预扫参数取 -C <dir> / --C <dir> / -C=<dir>(与其他 flag 的
// 顺序无关;需在任何路径相关初始化之前 chdir,故不走常规 flag 定义)。
func parseWorkspaceFlag(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		var v string
		switch {
		case a == "-C" || a == "--C":
			if i+1 >= len(args) {
				return "", fmt.Errorf("-C requires a directory argument")
			}
			v = args[i+1]
			i++
		case strings.HasPrefix(a, "-C=") || strings.HasPrefix(a, "--C="):
			v = strings.TrimPrefix(strings.TrimPrefix(a, "-C="), "--C=")
		default:
			continue
		}
		if v == "" {
			return "", fmt.Errorf("-C: empty directory")
		}
		return v, nil
	}
	return "", nil
}
