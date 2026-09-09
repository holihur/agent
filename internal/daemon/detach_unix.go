//go:build unix

package daemon

import ("os/exec"; "syscall")

// detach 让子进程脱离控制终端(新会话),父进程退出后继续常驻。
func detach(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
