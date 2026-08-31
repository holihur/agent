// Package agent 是嵌入式使用的公开门面:外部 Go 程序经此在进程内构造并驱动
// agent,无需 CLI、REPL 或 flag 解析。
//
// 设计原则:不堆参数。凭据与模型走 Config 字段或 env(LLM_API_KEY / LLM_BASE_URL /
// LLM_MODEL / LLM_AUTH_STYLE),全部可选;能力(shell、MCP、进程内工具、流式)
// 一律经方法显式开启。本包只做装配与类型转发,循环/工具/协议实现保留在
// internal/ 中,分层纪律不变;cmd/agent 的 CLI 装配不受影响。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	core "github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/llm"
	"github.com/holihur/agent/internal/mcp"
	"github.com/holihur/agent/internal/session"
	"github.com/holihur/agent/internal/tools"
)

// TextDelta 是流式输出的文本增量(转发核心域同名类型)。
type TextDelta = core.TextDelta

// SessionStore 是会话持久化 port(转发核心域同名接口):
// 宿主可自定义实现(如 Redis/SQLite),默认用文件存储。
type SessionStore = core.SessionStore

// ErrSessionNotFound 表示指定会话不存在(转发核心域哨兵;用 errors.Is 判定)。
var ErrSessionNotFound = core.ErrSessionNotFound

// ToolFunc 是进程内工具的执行函数:入参为模型给出的 JSON 对象,返回回填文本。
// 返回 error 视为工具失败(转为 is_error 回填,不打断循环)。
type ToolFunc func(ctx context.Context, input json.RawMessage) (string, error)

// MCPSpec 描述一个 MCP 服务器:URL 非空时为 Streamable HTTP 远程服务器,
// 否则 Command 为 stdio 子进程(首元素是可执行文件,其余为参数)。
type MCPSpec struct {
	Name    string
	Command []string
	URL     string
	Env     map[string]string // 仅 stdio:追加到子进程继承环境之后
	Headers map[string]string // 仅远程
}

// Config 是可选装配配置:全部字段零值安全(空值回落 env 或内置默认)。
type Config struct {
	APIKey          string       // 空 → env LLM_API_KEY / LLM_APIKEY
	BaseURL         string       // 空 → env LLM_BASE_URL(Anthropic 兼容端点)
	Model           string       // 空 → env LLM_MODEL
	AuthStyle       string       // 空 → env LLM_AUTH_STYLE;bearer(默认) | x-api-key | both
	System          string       // 空 → 不注入 system prompt
	MaxTokens       int          // 0 → 1024
	MaxTurns        int          // 0 → 默认 60
	Temperature     *float64     // nil → 不发送(端点默认);指向 0 = 确定性;>1 报错
	ReasoningEffort string       // 空 → 不发送;常见 low/medium/high,值原样透传
	Sessions        SessionStore // nil → 默认文件存储(cwd 下 .agent/sessions)
}

// Agent 是嵌入式 agent 门面:New 构造,Tool/MCP/Shell 按需挂载,Run 驱动,
// Close 释放。所有方法并发不安全,宿主自行串行化。
type Agent struct {
	inner     *core.Agent
	registry  *tools.Registry
	local     *tools.LocalProvider
	sessions  core.SessionStore
	providers []*mcp.Provider // 经 MCP() 挂载的连接;Close 统一释放
}

// New 构造嵌入式 agent。零参数即纯 env 配置;至多传一个 Config 覆盖指定字段。
// 缺少任一凭据/APIKey、BaseURL、Model → fail-fast 报错。
func New(cfg ...Config) (*Agent, error) {
	if len(cfg) > 1 {
		return nil, errors.New("agent: New accepts at most one Config")
	}
	c := Config{}
	if len(cfg) == 1 {
		c = cfg[0]
	}
	apiKey := firstNonEmpty(c.APIKey, os.Getenv("LLM_API_KEY"), os.Getenv("LLM_APIKEY"))
	baseURL := firstNonEmpty(c.BaseURL, os.Getenv("LLM_BASE_URL"))
	model := firstNonEmpty(c.Model, os.Getenv("LLM_MODEL"))
	authStyle := firstNonEmpty(c.AuthStyle, os.Getenv("LLM_AUTH_STYLE"))
	switch {
	case apiKey == "":
		return nil, errors.New("agent: no API key: set Config.APIKey or env LLM_API_KEY")
	case baseURL == "":
		return nil, errors.New("agent: no base URL: set Config.BaseURL or env LLM_BASE_URL")
	case model == "":
		return nil, errors.New("agent: no model: set Config.Model or env LLM_MODEL")
	}

	local := tools.NewLocal()
	registry := tools.New()
	if err := registry.Register(local); err != nil {
		return nil, err
	}
	if c.Temperature != nil && *c.Temperature > 1 {
		return nil, fmt.Errorf("agent: temperature must be in [0,1], got %v", *c.Temperature)
	}
	client := llm.New(apiKey, baseURL, model, c.MaxTokens)
	client.AuthStyle = authStyle
	client.Temperature = c.Temperature
	client.ReasoningEffort = c.ReasoningEffort
	sessions := c.Sessions
	if sessions == nil {
		sessions = session.NewFileStore(".agent/sessions")
	}
	return &Agent{
		inner: &core.Agent{
			LLM:      client,
			Registry: registry,
			System:   c.System,
			Hooks:    core.NewHooks(),
			MaxTurns: c.MaxTurns,
		},
		registry: registry,
		local:    local,
		sessions: sessions,
	}, nil
}

// Tool 注册一个进程内函数工具(暴露名不加前缀);重名立即报错。
func (a *Agent) Tool(name, description string, schema map[string]any, fn ToolFunc) error {
	return a.local.Register(tools.ToolDef{Name: name, Description: description, InputSchema: schema}, fn)
}

// MCP 挂载一个 MCP 服务器并做连接预检:stdio 启动失败或远程不可达均 fail-fast。
// 工具追问(MRTR)默认一律拒绝;需要交互的宿主未来经 Responder 注入。
func (a *Agent) MCP(spec MCPSpec) error {
	if spec.Name == "" {
		return errors.New("agent: mcp spec requires a name")
	}
	var p *mcp.Provider
	switch {
	case spec.URL != "":
		p = mcp.NewHTTP(spec.Name, mcp.HTTPConfig{URL: spec.URL, Headers: spec.Headers}, noopResponder{})
	default:
		if len(spec.Command) == 0 {
			return fmt.Errorf("agent: mcp %q: URL or Command required", spec.Name)
		}
		p = mcp.NewStdio(spec.Name, mcp.StdioConfig{
			Command: spec.Command[0], Args: spec.Command[1:], Env: envPairs(spec.Env),
		}, noopResponder{})
	}
	if _, err := p.ListTools(context.Background()); err != nil {
		_ = p.Close()
		return fmt.Errorf("agent: mcp %q preflight: %w", spec.Name, err)
	}
	if err := a.registry.Register(p); err != nil {
		_ = p.Close()
		return err
	}
	a.providers = append(a.providers, p)
	return nil
}

// Shell 开启内置 shell 工具(sh -c,30s 超时,以宿主进程权限运行)。
// 嵌入式默认不开启;重复开启报错。
func (a *Agent) Shell() error {
	return tools.RegisterShell(a.local)
}

// OnTextDelta 注册流式文本增量消费者;nil 取消。适配器支持流式时每轮生效。
func (a *Agent) OnTextDelta(fn func(TextDelta)) {
	a.inner.OnTextDelta = fn
}

// Run 执行一轮完整"思考-行动-观察"循环,返回最终文本回答。
// 对话历史保留在 Agent 内,连续调用即多轮会话。
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	return a.inner.Run(ctx, input)
}

// ToolNames 返回当前暴露给模型的全部工具名(诊断用)。
func (a *Agent) ToolNames(ctx context.Context) ([]string, error) {
	defs, err := a.registry.Tools(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names, nil
}

// Close 释放全部 MCP 连接(幂等);返回首个释放错误。
func (a *Agent) Close() error {
	var first error
	for _, p := range a.providers {
		if err := p.Close(); err != nil && first == nil {
			first = err
		}
	}
	a.providers = nil
	return first
}

// SaveSession 把当前对话历史持久化为命名会话(整体替换同名旧会话)。
func (a *Agent) SaveSession(name string) error {
	return a.sessions.Save(context.Background(), name, a.inner.Messages)
}

// LoadSession 载入命名会话并替换当前对话历史(当前历史不保存,需要则先 SaveSession)。
func (a *Agent) LoadSession(name string) error {
	msgs, err := a.sessions.Load(context.Background(), name)
	if err != nil {
		return err
	}
	a.inner.Messages = msgs
	return nil
}

// SessionNames 列出全部已持久化的会话名(按名字排序)。
func (a *Agent) SessionNames() ([]string, error) {
	return a.sessions.Names(context.Background())
}

// DeleteSession 删除命名会话;不存在时返回包装的 ErrSessionNotFound。
func (a *Agent) DeleteSession(name string) error {
	return a.sessions.Delete(context.Background(), name)
}

// NewSession 清空当前对话历史,从零开始(已持久化的会话不受影响)。
func (a *Agent) NewSession() {
	a.inner.Messages = nil
}

// noopResponder 是无宿主交互时的默认追问应答者:一律拒绝(fail-fast 语义)。
type noopResponder struct{}

func (noopResponder) Respond(context.Context, tools.InputRequest) ([]tools.InputResponse, error) {
	return nil, errors.New("agent: no responder configured for tool input request")
}

// firstNonEmpty 返回首个非空实参;全空返回空串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// envPairs 把 env 映射转为键名排序的 "K=V" 切片(排序保证确定性)。
func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}
