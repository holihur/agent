package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// namePattern 同时满足 MCP 工具名建议(1-128,[A-Za-z0-9_.-])与
// Anthropic 工具名约束(^[a-zA-Z0-9_-]{1,128}$,不含点)—— 取两者交集。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// nsPattern 约束来源命名空间(不得含 __ 以免暴露名歧义;注册表不做反解析,校验保平安)。
var nsPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// localNamespace 是进程内函数工具的保留命名空间(暴露名不加前缀)。
const localNamespace = "local"

// Registry 聚合多个 Provider,向模型暴露统一的、带命名空间的工具平面。
type Registry struct {
	providers []Provider
	index     map[string]entry // exposed 名 → (provider, 原始名);每次 Tools() 重建
}

type entry struct {
	provider Provider
	rawName  string
}

func New() *Registry {
	return &Registry{index: map[string]entry{}}
}

// Register 注册一个工具来源;命名空间冲突立即报错(fail-fast)。
func (r *Registry) Register(p Provider) error {
	ns := p.Namespace()
	if !nsPattern.MatchString(ns) {
		return fmt.Errorf("tools: invalid provider namespace %q", ns)
	}
	for _, existing := range r.providers {
		if existing.Namespace() == ns {
			return fmt.Errorf("tools: duplicate provider namespace %q", ns)
		}
	}
	r.providers = append(r.providers, p)
	return nil
}

// Tools 聚合所有来源的工具,做命名空间投影与名字校验,并重建 Call 索引。
// 任何来源失败 → 整体失败(fail-fast,由上层决定致命性)。
func (r *Registry) Tools(ctx context.Context) ([]ToolDef, error) {
	var out []ToolDef
	r.index = map[string]entry{}
	for _, p := range r.providers {
		defs, err := p.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("tools: provider %q: %w", p.Namespace(), err)
		}
		for _, d := range defs {
			exposed := d.Name
			if p.Namespace() != localNamespace {
				exposed = p.Namespace() + "__" + d.Name
			}
			if !namePattern.MatchString(exposed) {
				return nil, fmt.Errorf(
					"tools: exposed name %q (provider %q, tool %q) is not usable with the Anthropic API",
					exposed, p.Namespace(), d.Name)
			}
			if _, dup := r.index[exposed]; dup {
				return nil, fmt.Errorf("tools: duplicate exposed tool name %q", exposed)
			}
			r.index[exposed] = entry{provider: p, rawName: d.Name}
			out = append(out, ToolDef{Name: exposed, Description: d.Description, InputSchema: d.InputSchema})
		}
	}
	return out, nil
}

// Call 按暴露名路由到对应 Provider;未知工具返回 error(调用方转为 is_error 回填)。
// 索引由最近一次 Tools() 建立 —— Agent 的投影→调用顺序保证其新鲜度。
func (r *Registry) Call(ctx context.Context, exposedName string, input json.RawMessage) (ToolResult, error) {
	e, ok := r.index[exposedName]
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown tool %q", exposedName)
	}
	return e.provider.CallTool(ctx, e.rawName, input)
}
