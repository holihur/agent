//go:build unix

// Package daemon 提供 agent MCP 服务器的后台守护进程方式:
// Start 以分离进程启动 `agent -addr ...`,Stop 按 pidfile 结束它。
// pidfile/log 相对当前 cwd(.agent/ 下),daemon 服务的工作目录即启动时的 cwd。
package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PidFile 返回 cwd 下的 pid 文件路径;LogFile 为日志文件路径。
func PidFile() string { return filepath.Join(".agent", "agent-mcp.pid") }
func LogFile() string { return filepath.Join(".agent", "agent-mcp.log") }

// alive 探测 pid 对应进程是否存活(signal 0)。
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// readPID 读取 pidfile;不存在返回 (0, nil)。
func readPID() (int, error) {
	b, err := os.ReadFile(PidFile())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("bad pidfile %s: %w", PidFile(), err)
	}
	return pid, nil
}

// Start 以分离进程启动 agent MCP 服务器并立即返回。
// args 为透传给子进程的 flag(如 -addr、-mdns、-name)。
// 已有存活的守护进程时幂等返回 (false, nil)。
func Start(args ...string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(PidFile()), 0o755); err != nil {
		return false, err
	}
	if pid, err := readPID(); err == nil && alive(pid) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	logF, err := os.OpenFile(LogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, err
	}
	defer logF.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logF, logF
	cmd.Stdin, cmd.Env = nil, os.Environ()
	if err := detach(cmd); err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()

	// 落 pidfile 前稍候,尽早暴露监听失败(端口占用等)。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if !alive(pid) {
			return false, fmt.Errorf("daemon exited immediately; see %s", LogFile())
		}
	}
	return true, os.WriteFile(PidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// Stop 结束守护进程并清理 pidfile;未在运行时幂等返回 false。
func Stop() (bool, error) {
	pid, err := readPID()
	if err != nil {
		return false, err
	}
	if pid == 0 || !alive(pid) {
		_ = os.Remove(PidFile())
		return false, nil
	}
	// 进程组整体结束(Start 已 detach 成独立会话/进程组)。
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			return false, err
		}
	}
	// 等待退出,超时升级 SIGKILL。
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = os.Remove(PidFile())
	return true, nil
}
