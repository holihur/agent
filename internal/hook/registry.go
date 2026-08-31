// Package hook 提供 Agent 生命周期钩子的功能安装器。
//
// 契约(Hooks 类型)在 internal/agent/hooks.go;本包内每个功能一个文件,
// 自声明 CLI flag 并在 init 中 Register,装配层(main)只调一次 InstallAll
// —— 新增钩子功能 = 在本包新增一个文件,main.go 零改动。
package hook

import (
	"fmt"
	"maps"
	"slices"

	"agent/internal/agent"
)

// Installer 是一个功能的钩子装配动作:把该功能的钩子注册到 h 上。
// flag 关闭时安装器应安静地返回 nil。
type Installer func(h *agent.Hooks, d Deps) error

// Deps 是全部安装器共享的装配依赖;字段增长需保持与具体功能解耦。
type Deps struct {
	CWD string    // 启动工作目录(逐层发现类功能用)
	UI  ConfirmUI // 会话界面;nil = 不装需要用户交互的功能
}

// ConfirmUI 由会话 UI 实现,为 confirm-tool 提供逐次放行裁决。
type ConfirmUI interface {
	ConfirmHook() func(agent.ToolCall) agent.Decision
}

var registry = map[string]Installer{}

// Register 登记一个功能安装器;重名即 panic(init 期编程错误,越早炸越好)。
func Register(name string, fn Installer) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("hook: duplicate installer %q", name))
	}
	registry[name] = fn
}

// InstallAll 按名字序执行全部已登记安装器;任一失败即 fail-fast。
func InstallAll(h *agent.Hooks, d Deps) error {
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		if err := registry[name](h, d); err != nil {
			return fmt.Errorf("hook %s: %w", name, err)
		}
	}
	return nil
}
