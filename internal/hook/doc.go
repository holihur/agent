// Package hook 提供 Agent 生命周期钩子的功能安装器:verbose 观测日志、
// AGENTS.md 指令注入等。契约(Hooks 类型)在 internal/agent/hooks.go,
// 本包只做"功能 → 钩子"的装配,由 cmd/agent 在启动时调用。
package hook
