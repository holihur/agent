package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/holihur/agent/internal/tools"
)

// 协议常量(规范 §basic/index Per-request protocol fields)。
// 缺少必填 _meta 字段 → 服务器必须回 -32602,所以每个请求都注入。
const (
	protocolVersion     = "2026-07-28"
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo      = "io.modelcontextprotocol/clientInfo"
	metaClientCaps      = "io.modelcontextprotocol/clientCapabilities"

	resultTypeComplete      = "complete"
	resultTypeInputRequired = "input_required"

	// legacy(2025-06-18)语义:initialize 握手建立的会话,作用于 stdio 进程生命周期。
	// 生态现实:当前已发布的大多数服务器仍是 legacy;规范为此定义了
	// dual-era 客户端的探测与回退路径(§stdio Backward Compatibility)。
	eraModern = "modern"
	eraLegacy = "legacy"

	legacyProtocolVersion = "2025-06-18"
)

// maxMRTRRounds 是单次工具调用的追问轮数上限(防无限追问)。
const maxMRTRRounds = 3

// probeTimeout 是时代探测/握手超时(规范:reasonable timeout)。
const probeTimeout = 10 * time.Second

// Provider 实现 tools.Provider:一个 MCP 服务器即一个工具源。
// 无状态红利:子进程崩溃不留会话,下次调用按需重新 spawn(规范 §stdio Unexpected Termination)。
type Provider struct {
	name      string
	dial      DialFunc
	responder tools.Responder // MRTR 追问的应答者(cmd 注入 CLI 实现);nil = 不支持交互

	era    string     // 服务器时代(modern/legacy);探测结果按规范缓存
	mu     sync.Mutex // 以下字段由 mu 保护
	client *rpcClient
	cache  []tools.ToolDef
	expiry time.Time
}

// NewStdio 创建基于子进程 stdio 传输的 MCP Provider。
func NewStdio(name string, cfg StdioConfig, responder tools.Responder) *Provider {
	return &Provider{
		name:      name,
		dial:      func(_ context.Context) (Transport, error) { return dialStdio(context.Background(), cfg) },
		responder: responder,
	}
}

// NewHTTP 创建基于 Streamable HTTP 传输的 MCP Provider(规范 §mcp.json/remote)。
// 与 stdio 共用同一套时代探测与按需重生:dial 是幂等的建连,连接无子进程生命周期。
func NewHTTP(name string, cfg HTTPConfig, responder tools.Responder) *Provider {
	return &Provider{
		name:      name,
		dial:      func(_ context.Context) (Transport, error) { return dialHTTP(context.Background(), cfg) },
		responder: responder,
	}
}

// Namespace 实现 tools.Provider。
func (p *Provider) Namespace() string { return p.name }

// Close 终止当前连接(若有)。进程随 stdin 关闭退出;超时强杀。
func (p *Provider) Close() error {
	p.mu.Lock()
	c := p.client
	p.client = nil
	p.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// ---- 连接管理(按需重生) ----

// conn 返回可用连接;已死或未建立时按需建立并做时代探测。
func (p *Provider) conn(ctx context.Context) (*rpcClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		select {
		case <-p.client.done:
			p.client = nil // 上次连接已死 → 重生
		default:
			return p.client, nil
		}
	}
	tr, err := p.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: dial server %q: %w", p.name, err)
	}
	c := newRPCClient(tr)
	if p.era == "" {
		era, herr := p.handshake(ctx, c)
		if herr != nil {
			_ = c.Close()
			return nil, herr
		}
		p.era = era
	} else if p.era == eraLegacy {
		// legacy 会话作用于进程生命周期:重生后的新进程必须重新握手。
		if err := legacyInitialize(ctx, c); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("mcp: re-initialize server %q: %w", p.name, err)
		}
	}
	p.client = c
	return c, nil
}

// handshake 判定服务器时代并完成相应初始化(规范 §stdio Backward Compatibility):
//   - server/discover 成功            → modern(无状态)
//   - UnsupportedProtocolVersionError → modern 但无共同版本,报错并列出对方 supported
//   - 其他错误(常见 -32601)           → legacy,回退 initialize 握手
func (p *Provider) handshake(ctx context.Context, c *rpcClient) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := c.call(pctx, "server/discover", withMeta(map[string]any{}))
	if err == nil {
		var disc struct {
			SupportedVersions []string `json:"supportedVersions"`
		}
		_ = json.Unmarshal(res, &disc) // 形状宽松:能解析多少算多少
		if len(disc.SupportedVersions) > 0 && !contains(disc.SupportedVersions, protocolVersion) {
			return "", fmt.Errorf("mcp: server %q supports %v, not %s", p.name, disc.SupportedVersions, protocolVersion)
		}
		return eraModern, nil
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) && rpcErr.Code == codeUnsupportedProtocolVersion {
		return "", fmt.Errorf("mcp: server %q does not support %s (supported: %s)",
			p.name, protocolVersion, supportedList(rpcErr.Data))
	}
	if err := legacyInitialize(ctx, c); err != nil {
		return "", fmt.Errorf("mcp: server %q is neither a modern (%s) nor a workable legacy MCP server: %w",
			p.name, protocolVersion, err)
	}
	return eraLegacy, nil
}

// legacyInitialize 完成 2025-06-18 语义的 initialize 握手。
// HTTP 传输在此把协议版本头回写为 legacy:进入本函数即已确认服务器非 modern,
// 后续所有请求(含 initialize 自身)都应携带服务器认识的版本。
func legacyInitialize(ctx context.Context, c *rpcClient) error {
	if vs, ok := c.tr.(interface{ setProtocolVersion(string) }); ok {
		vs.setProtocolVersion(legacyProtocolVersion)
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := c.call(pctx, "initialize", map[string]any{
		"protocolVersion": legacyProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agent", "version": "0.1.0"},
	})
	if err != nil {
		return err
	}
	var ir struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(res, &ir)
	if ir.ProtocolVersion != "" && ir.ProtocolVersion != legacyProtocolVersion {
		return fmt.Errorf("server negotiated unsupported version %q", ir.ProtocolVersion)
	}
	return c.notify("notifications/initialized", nil)
}

// ---- tools.Provider 实现 ----

// ListTools 列出服务器工具:cursor 分页取尽;按结果 ttlMs 缓存(规范 §utilities/caching)。
func (p *Provider) ListTools(ctx context.Context) ([]tools.ToolDef, error) {
	p.mu.Lock()
	if p.cache != nil && time.Now().Before(p.expiry) {
		cached := p.cache
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	var defs []tools.ToolDef
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := p.doCall(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
			TTLms      int64  `json:"ttlMs"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
		}
		for _, t := range page.Tools {
			schema := t.InputSchema
			if schema == nil {
				// 规范:inputSchema MUST 是合法 JSON Schema 对象;缺省兜底为空对象模式。
				schema = map[string]any{"type": "object"}
			}
			defs = append(defs, tools.ToolDef{Name: t.Name, Description: t.Description, InputSchema: schema})
		}
		if page.NextCursor == "" {
			if page.TTLms > 0 {
				p.mu.Lock()
				p.cache = defs
				p.expiry = time.Now().Add(time.Duration(page.TTLms) * time.Millisecond)
				p.mu.Unlock()
			}
			return defs, nil
		}
		cursor = page.NextCursor
	}
}

// CallTool 执行 tools/call,内含 MRTR 闭环:
// 收到 resultType=input_required → 调 Responder → 携带 inputResponses 重试(新 JSON-RPC id,≤3 轮)。
func (p *Provider) CallTool(ctx context.Context, name string, input json.RawMessage) (tools.ToolResult, error) {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	var inputResponses map[string]any
	var requestState string

	for round := 0; ; round++ {
		params := map[string]any{"name": name, "arguments": json.RawMessage(input)}
		if inputResponses != nil {
			params["inputResponses"] = inputResponses
			if requestState != "" {
				params["requestState"] = requestState
			}
		}
		res, err := p.doCall(ctx, "tools/call", params)
		if err != nil {
			return tools.ToolResult{}, err
		}
		var result callResult
		if err := json.Unmarshal(res, &result); err != nil {
			return tools.ToolResult{}, fmt.Errorf("mcp: decode tools/call result: %w", err)
		}
		switch result.ResultType {
		case "", resultTypeComplete: // 规范:缺省 resultType 视为 complete
			return mapToolResult(result), nil
		case resultTypeInputRequired:
			if round >= maxMRTRRounds {
				return tools.ToolResult{IsError: true,
					Text: "error: tool asked for input too many times"}, nil
			}
			if p.responder == nil {
				return tools.ToolResult{IsError: true,
					Text: "error: tool requires interactive input, but no responder is configured"}, nil
			}
			resp, err := p.responder.Respond(ctx, projectInputRequests(name, result.InputRequests))
			if err != nil {
				return tools.ToolResult{IsError: true,
					Text: "error: user declined to answer the tool's question: " + err.Error()}, nil
			}
			inputResponses = buildInputResponses(resp)
			requestState = result.RequestState
		default:
			// 规范:客户端不认识的 resultType 视为 invalid。
			return tools.ToolResult{}, fmt.Errorf("mcp: unknown resultType %q", result.ResultType)
		}
	}
}

// ---- 内部机制 ----

// doCall 建立连接后执行一次 RPC。
// _meta 注入必须发生在 conn() 之后 —— era 由握手判定,先连接后构造请求元数据。
func (p *Provider) doCall(ctx context.Context, method string, base map[string]any) (json.RawMessage, error) {
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.call(ctx, method, p.injectMeta(base))
	if err != nil {
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			p.dropConn()
		}
		return nil, fmt.Errorf("mcp: %s on %q: %w", method, p.name, err)
	}
	return res, nil
}

func (p *Provider) dropConn() {
	p.mu.Lock()
	p.client = nil
	p.mu.Unlock()
}

// withMeta 注入 modern 请求必带的 _meta(规范 §basic/index)。
func withMeta(base map[string]any) map[string]any {
	base["_meta"] = map[string]any{
		metaProtocolVersion: protocolVersion,
		metaClientCaps:      map[string]any{}, // 本客户端不声明任何能力
		metaClientInfo:      map[string]any{"name": "agent", "version": "0.1.0"},
	}
	return base
}

// injectMeta 按服务器时代决定请求元数据:
// modern 每请求带 _meta;legacy 请求不带(其语义来自 initialize 会话)。
func (p *Provider) injectMeta(base map[string]any) map[string]any {
	p.mu.Lock()
	era := p.era
	p.mu.Unlock()
	if era == eraLegacy {
		return base
	}
	return withMeta(base)
}

// ---- MCP wire 类型(仅本包可见) ----

// wireContent 是工具结果的内容块(只解析文本管线需要的字段)。
type wireContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"resource"`
}

// wireInputRequest 对应 MRTR inputRequests map 的值(elicitation 形状)。
type wireInputRequest struct {
	Method string `json:"method"`
	Params struct {
		Mode            string `json:"mode"`
		Message         string `json:"message"`
		RequestedSchema struct {
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"requestedSchema"`
	} `json:"params"`
}

type callResult struct {
	ResultType    string                      `json:"resultType"`
	Content       []wireContent               `json:"content"`
	IsError       bool                        `json:"isError"`
	Structured    json.RawMessage             `json:"structuredContent"`
	InputRequests map[string]wireInputRequest `json:"inputRequests"`
	RequestState  string                      `json:"requestState"`
}

// mapToolResult 把 CallToolResult 映射为领域 ToolResult(设计 v3 §五映射表)。
func mapToolResult(r callResult) tools.ToolResult {
	var parts []string
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s: %s omitted]", c.Type, c.MimeType))
		case "resource_link":
			parts = append(parts, "resource_link: "+c.URI)
		case "resource":
			if c.Resource != nil {
				if c.Resource.Text != "" {
					parts = append(parts, c.Resource.Text)
				} else {
					parts = append(parts, "resource: "+c.Resource.URI)
				}
			}
		default:
			parts = append(parts, fmt.Sprintf("[%s: unsupported content type]", c.Type))
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" && len(r.Structured) > 0 {
		text = string(r.Structured) // 结构化结果兜底为序列化 JSON
	}
	return tools.ToolResult{Text: text, IsError: r.IsError}
}

// projectInputRequests 把 MRTR 的 inputRequests 投影为领域追问(确定性顺序)。
func projectInputRequests(toolName string, reqs map[string]wireInputRequest) tools.InputRequest {
	req := tools.InputRequest{Tool: toolName}
	keys := make([]string, 0, len(reqs))
	for k := range reqs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r := reqs[key]
		prompt := tools.InputPrompt{Key: key, Message: r.Params.Message}
		required := map[string]bool{}
		for _, f := range r.Params.RequestedSchema.Required {
			required[f] = true
		}
		fields := make([]string, 0, len(r.Params.RequestedSchema.Properties))
		for name := range r.Params.RequestedSchema.Properties {
			fields = append(fields, name)
		}
		sort.Strings(fields)
		for _, name := range fields {
			prop := r.Params.RequestedSchema.Properties[name]
			prompt.Fields = append(prompt.Fields, tools.InputField{
				Name: name, Description: prop.Description, Required: required[name],
			})
		}
		req.Prompts = append(req.Prompts, prompt)
	}
	return req
}

// buildInputResponses 把用户作答转为规范要求的重试载荷(action=accept + content)。
func buildInputResponses(resps []tools.InputResponse) map[string]any {
	out := map[string]any{}
	for _, r := range resps {
		content := r.Content
		if content == nil {
			content = map[string]any{}
		}
		out[r.Key] = map[string]any{"action": "accept", "content": content}
	}
	return out
}

func contains(vs []string, s string) bool {
	for _, v := range vs {
		if v == s {
			return true
		}
	}
	return false
}

func supportedList(data json.RawMessage) string {
	var d struct {
		Supported []string `json:"supported"`
	}
	if json.Unmarshal(data, &d) == nil && len(d.Supported) > 0 {
		return strings.Join(d.Supported, ", ")
	}
	return "unknown"
}
