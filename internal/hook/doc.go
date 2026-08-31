// Package hook 提供 Agent 生命周期钩子的功能安装器。
//
// 每个 hook 是 internal/hook/ 下的一个独立子包(agentsmd、confirm、
// verbose、shell……),各自声明 CLI flag 并在自己的 init 中 Register;
// 由 cmd/agent blank-import 激活,启动时统一调一次 InstallAll。
package hook
