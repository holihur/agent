package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// StdioConfig 描述一个通过子进程启动的 MCP 服务器。
type StdioConfig struct {
	Command string
	Args    []string
	Env     []string // 追加到子进程环境的键值对("K=V")
}

// errClosed 标记传输已被主动关闭(区别于进程异常退出)。
var errClosed = errors.New("mcp: transport closed")

// maxFrameSize 单帧上限(防失控服务器耗尽内存)。
const maxFrameSize = 4 << 20

// shutdownGrace 是 Close 时等待服务器自行退出的时间,超过则强杀
// (规范 §stdio Shutdown:关 stdin → 等待 → 升级终止)。
const shutdownGrace = 3 * time.Second

// dialStdio 启动子进程并返回 NDJSON 传输。
// 使用独立于请求的进程生命周期:进程存活期 > 单次请求
// (规范 §Statelessness:连接不是会话)。
func dialStdio(_ context.Context, cfg StdioConfig) (Transport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = append(os.Environ(), cfg.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	// 规范 §stdio:stderr 仅为日志,客户端 MAY 忽略;丢弃以保证宿主输出纯净。
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %q: %w", cfg.Command, err)
	}
	t := &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		frames: make(chan []byte, 16),
		doneCh: make(chan struct{}),
		closed: make(chan struct{}),
	}
	go t.readLoop(stdout)
	go t.waitLoop()
	return t, nil
}

// stdioTransport 实现 Transport:子进程 stdin/stdout 上的 NDJSON 帧。
type stdioTransport struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	frames   chan []byte
	doneCh   chan struct{} // 读循环结束(进程退出)
	closed   chan struct{} // 主动关闭
	errOnce  sync.Once
	closeOne sync.Once
	writeMu  sync.Mutex
	err      error
}

func (t *stdioTransport) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrameSize)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		frame := make([]byte, len(line))
		copy(frame, line)
		select {
		case t.frames <- frame:
		case <-t.closed:
			return
		}
	}
	// scanner 结束(EOF 或读错误):由 waitLoop 统一定格退出原因。
}

func (t *stdioTransport) waitLoop() {
	werr := t.cmd.Wait()
	rerr := error(io.EOF)
	if werr != nil {
		rerr = fmt.Errorf("mcp: server exited: %w", werr)
	}
	t.errOnce.Do(func() { t.err = rerr })
	close(t.doneCh)
}

func (t *stdioTransport) Send(b []byte) error {
	select {
	case <-t.closed:
		return errClosed
	default:
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	return nil
}

func (t *stdioTransport) Recv() ([]byte, error) {
	for {
		// 先排空已缓冲帧,再阻塞等待(避免与 doneCh 的随机竞争)。
		select {
		case b := <-t.frames:
			return b, nil
		default:
		}
		select {
		case b := <-t.frames:
			return b, nil
		case <-t.doneCh:
			select { // doneCh 与最后一帧可能同时就绪:再排空一次
			case b := <-t.frames:
				return b, nil
			default:
			}
			if t.err != nil {
				return nil, t.err
			}
			return nil, io.EOF
		case <-t.closed:
			return nil, errClosed
		}
	}
}

// Close 按规范顺序关停:关闭 stdin → 等待退出 → 超时强杀。
func (t *stdioTransport) Close() error {
	t.closeOne.Do(func() { close(t.closed) })
	_ = t.stdin.Close()
	select {
	case <-t.doneCh:
	case <-time.After(shutdownGrace):
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		<-t.doneCh
	}
	return nil
}
