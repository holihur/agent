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
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"agent/internal/agent"
	"agent/internal/llm"
	"agent/internal/mcp"
	"agent/internal/tools"
	uicli "agent/internal/ui/cli"
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
		servers     mcpFlags
		quick       = flag.String("q", "", "one-shot question (default: interactive REPL)")
		system      = flag.String("system", defaultSystem, "system prompt")
		provider    = flag.String("provider", "", "env prefix: NAME reads NAME_API_KEY/NAME_APIKEY, NAME_BASE_URL, NAME_MODEL")
		model       = flag.String("model", "", "model override (default: LLM_MODEL or NAME_MODEL)")
		maxToks     = flag.Int("max-tokens", 1024, "max_tokens per LLM turn")
		confirmTool = flag.Bool("confirm-tool", false, "ask before every tool call (hook)")
		verbose     = flag.Bool("verbose", false, "print LLM turns and tool outcomes to stderr (hook)")
	)
	flag.Var(&servers, "mcp", "MCP stdio server, repeatable: <name>=<command> [args...]")
	flag.Parse()

	loadDotEnv(".env")

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

	// 生命周期钩子:功能扩展的统一缝隙(见 internal/hook 包契约)。
	hooks := agent.NewHooks()
	if *verbose {
		installVerboseHooks(hooks)
	}
	if *confirmTool {
		hooks.OnBeforeTool(ui.ConfirmHook())
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
	ag := &agent.Agent{LLM: llmClient, Registry: registry, System: *system, Hooks: hooks}

	ui.Agent = ag // 两阶段装配:Responder(即 UI)先于 Agent 可用

	if *quick != "" {
		answer, err := ag.Run(ctx, *quick)
		if err != nil {
			return err
		}
		fmt.Println(answer)
		return nil
	}
	return ui.Run(ctx)
}

// installVerboseHooks 把 LLM 轮次与工具调用的观测日志挂到 stderr。
func installVerboseHooks(h *agent.Hooks) {
	h.OnBeforeLLM(func(s agent.TurnStat) {
		fmt.Fprintf(os.Stderr, "[llm] turn %d: messages=%d tools=%d\n", s.Turn, s.Messages, s.Tools)
	})
	h.OnAfterLLM(func(s agent.TurnStat) {
		fmt.Fprintf(os.Stderr, "[llm] turn %d: stop=%s blocks=%d\n", s.Turn, s.StopReason, s.Blocks)
	})
	h.OnBeforeTool(func(c agent.ToolCall) agent.Decision {
		fmt.Fprintf(os.Stderr, "[tool] %s(%s)\n", c.Name, string(c.Input))
		return agent.Decision{}
	})
	h.OnAfterTool(func(o agent.ToolOutcome) {
		status := "ok"
		switch {
		case o.Denied:
			status = "denied"
		case o.IsError:
			status = "error"
		}
		fmt.Fprintf(os.Stderr, "[tool] %s -> %s (%s)\n", o.Name, status, o.Duration.Round(time.Millisecond))
	})
}

func envFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}
