// Package memory 实现长期记忆的默认启用 hook:启动时以 cwd 下 .agent/memory/
// 为根挂上文件记忆存储,注册 memory_save/search/forget 工具,并把记忆键文件树
// 注入每轮 system prompt。flag -memory 可改目录或 off/none 整体禁用。
package memory

import (
	"context"
	"errors"
	"flag"
	"path/filepath"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	memstore "github.com/holihur/agent/internal/memory"
)

// defaultMemoryDir 是 -memory 的默认记忆目录(相对 cwd)。
const defaultMemoryDir = ".agent/memory"

var memoryDir = flag.String("memory", defaultMemoryDir,
	"long-term memory directory (relative to cwd or absolute); off/none disables (hook)")

func init() {
	hook.Register("memory", installMemory)
}

// installMemory 装配长期记忆:注册工具 + 每轮 system prompt 注入记忆索引。
func installMemory(h *agent.Hooks, d hook.Deps) error {
	mode := *memoryDir
	if mode == "off" || mode == "none" {
		return nil
	}
	dir := mode
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(d.CWD, dir)
	}
	store := memstore.NewFileStore(dir)
	if d.Tools == nil {
		return errors.New("memory hook needs the local tool provider (Deps.Tools)")
	}
	if err := memstore.RegisterTools(d.Tools, store); err != nil {
		return err
	}
	h.OnMutateTurnRequest(func(r agent.TurnRequest) agent.TurnRequest {
		keys, err := store.Keys(context.Background())
		if err != nil || len(keys) == 0 {
			return r
		}
		r.System += memstore.IndexPrompt(keys)
		return r
	})
	return nil
}
