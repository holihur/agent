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
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/llm"
	"github.com/holihur/agent/internal/mcp"
	"github.com/holihur/agent/internal/tools"
	uicli "github.com/holihur/agent/internal/ui/cli"
	"github.com/holihur/agent/internal/utils"

	// 钩子功能包:各自在 init 中向 hook 注册(新增功能 = 新增子目录 + 此处一行)。
	_ "github.com/holihur/agent/internal/hook/agentsmd"
	_ "github.com/holihur/agent/internal/hook/confirm"
	_ "github.com/holihur/agent/internal/hook/shell"
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
		return fmt.Errorf("-mcp must be <name>=<command> [args...], got %q", v)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("-mcp %s: missing command", name)
	}
	*f = append(*f, mcpServer{Name: name, Command: fields[0], Args: fields[1:]})
	return nil
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
	)
	flag.Var(&servers, "mcp", "MCP stdio server, repeatable: <name>=<command> [args...]")
	flag.Parse()

	utils.LoadDotEnv(".env")

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

	registry := tools.New()
	builtin, err := tools.NewBuiltin()
	if err != nil {
		return err
	}
	if err := registry.Register(builtin); err != nil {
		return err
	}

	ui := uicli.New(os.Stdin, os.Stdout)

	// 生命周期钩子:每个 hook 是 internal/hook/ 下一个子包,init 自注册,
	// 上方 blank-import 激活;这里只统一装配 InstallAll。
	hooks := agent.NewHooks()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := hook.InstallAll(hooks, hook.Deps{CWD: cwd, UI: ui}); err != nil {
		return err
	}

	// MCP 服务器:每个是一个 Provider;REPL UI 同时充当 MRTR 应答者。
	var mcpProviders []*mcp.Provider
	for _, s := range servers {
		p := mcp.NewStdio(s.Name, mcp.StdioConfig{Command: s.Command, Args: s.Args}, ui)
		mcpProviders = append(mcpProviders, p)
		if err := registry.Register(p); err != nil {
			return err
		}
	}
	defer func() {
		for _, p := range mcpProviders {
			_ = p.Close()
		}
	}()

	// 预检:聚合工具列表(连接各服务器 / 暴露名冲突,全部 fail-fast)。
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
	ag := &agent.Agent{LLM: llmClient, Registry: registry, System: *system, Hooks: hooks, MaxTurns: *maxTurns}

	ui.Agent = ag                       // 两阶段装配:Responder(即 UI)先于 Agent 可用
	ag.OnTextDelta = ui.TextDeltaSink() // 流式增量 → 终端

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
