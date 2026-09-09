// tools.go 把长期记忆暴露为模型可调用的工具(memory_save/search/forget),
// 并提供注入 system prompt 的记忆键文件树 —— 门面(agent.go)与 CLI hook
// (internal/hook/memory)共用同一份实现,避免双份漂移。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/holihur/agent/internal/tools"
)

// Store 是注册工具所需的最小记忆接口(Put/Search/Delete);core.Memory 天然满足。
type Store interface {
	Put(ctx context.Context, key, value string) error
	Search(ctx context.Context, query string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// Registerer 是工具注册面的最小接口;*tools.LocalProvider 满足。
type Registerer interface {
	Register(def tools.ToolDef, fn func(ctx context.Context, input json.RawMessage) (string, error)) error
}

// RegisterTools 向进程内工具平面暴露 memory_save / memory_search / memory_forget
// 三个工具;读取单条记忆走宿主门面方法,不经工具(与门面契约一致)。
func RegisterTools(r Registerer, m Store) error {
	saveSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key":   map[string]any{"type": "string", "description": "记忆键,[a-zA-Z0-9_-],1-64"},
			"value": map[string]any{"type": "string", "description": "记忆内容,非空"},
		},
		"required": []string{"key", "value"},
	}
	searchSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "子串查询,空串列出全部键"},
		},
		"required": []string{"query"},
	}
	forgetSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{"type": "string", "description": "要删除的记忆键"},
		},
		"required": []string{"key"},
	}
	type memInput struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		Query string `json:"query"`
	}
	unmarshal := func(input json.RawMessage) (memInput, error) {
		var in memInput
		if err := json.Unmarshal(input, &in); err != nil {
			return in, fmt.Errorf("memory tool: bad input: %w", err)
		}
		return in, nil
	}
	if err := r.Register(tools.ToolDef{
		Name: "memory_save", Description: "保存一条长期记忆(按 key 覆盖)",
		InputSchema: saveSchema,
	}, func(ctx context.Context, raw json.RawMessage) (string, error) {
		in, err := unmarshal(raw)
		if err != nil {
			return "", err
		}
		if err := m.Put(ctx, in.Key, in.Value); err != nil {
			return "", err
		}
		return "saved: " + in.Key, nil
	}); err != nil {
		return err
	}
	if err := r.Register(tools.ToolDef{
		Name: "memory_search", Description: "按子串检索长期记忆键,空查询列出全部键",
		InputSchema: searchSchema,
	}, func(ctx context.Context, raw json.RawMessage) (string, error) {
		in, err := unmarshal(raw)
		if err != nil {
			return "", err
		}
		keys, err := m.Search(ctx, in.Query)
		if err != nil {
			return "", err
		}
		if len(keys) == 0 {
			return "no matches", nil
		}
		return strings.Join(keys, "\n"), nil
	}); err != nil {
		return err
	}
	return r.Register(tools.ToolDef{
		Name: "memory_forget", Description: "删除一条长期记忆",
		InputSchema: forgetSchema,
	}, func(ctx context.Context, raw json.RawMessage) (string, error) {
		in, err := unmarshal(raw)
		if err != nil {
			return "", err
		}
		if err := m.Delete(ctx, in.Key); err != nil {
			return "", err
		}
		return "deleted: " + in.Key, nil
	})
}

// MaxTreeKeys 是注入 system prompt 的记忆键上限:超出截断并注明总数,
// 保证"简介高速"——记忆列表不随规模膨胀挤占上下文。
const MaxTreeKeys = 200

// Tree 把扁平记忆键列表('-' 作层级分隔)渲染为文件树文本。
// 例:"lang-go" → lang/go.json 的树形缩进;超过 MaxTreeKeys 截断。
func Tree(keys []string) string {
	var b strings.Builder
	b.WriteString(".agent/memory/\n")
	for i, k := range keys {
		if i >= MaxTreeKeys {
			fmt.Fprintf(&b, "… (+%d more, %d total)\n", len(keys)-MaxTreeKeys, len(keys))
			break
		}
		depth := strings.Count(k, "-")
		indent := strings.Repeat("  ", depth)
		name := strings.ReplaceAll(k, "-", "/") + ".json"
		fmt.Fprintf(&b, "%s%s\n", indent, name)
	}
	return b.String()
}

// IndexPrompt 是每轮追加到 system prompt 的记忆索引段:头行说明 + 记忆文件树。
func IndexPrompt(keys []string) string {
	return "\n\n# Long-term memory index\nMemories are stored as files; the tree below shows what exists (key segments separated by \"-\"). Use the memory tools to read/save/delete individual entries.\n\n" + Tree(keys)
}
