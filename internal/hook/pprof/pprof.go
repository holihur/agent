// Package pprof 实现按需开启的 net/http/pprof 诊断端点 hook:
// -pprof <addr>(on = localhost:6060)在后台起 HTTP 服务,默认关闭。
// 绑定失败 fail-fast;服务随进程存活,无优雅关闭(诊断用途,进程退出即结束)。
package pprof

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // 注册 /debug/pprof/* handler 到 DefaultServeMux
	"os"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

var pprofAddr = flag.String("pprof", "", `serve net/http/pprof on <addr> (e.g. localhost:6060); on = localhost:6060; empty/off/none disables (hook)`)

func init() {
	hook.Register("pprof", installPprof)
}

// servingAddr 是实际绑定的地址(随机端口时与请求地址不同);空 = 未开启。
var servingAddr string

// installPprof 按需绑定地址并后台 Serve;绑定失败 fail-fast。
func installPprof(_ *agent.Hooks, _ hook.Deps) error {
	addr := resolveAddr(*pprofAddr)
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	servingAddr = ln.Addr().String()
	fmt.Fprintf(os.Stderr, "pprof: serving on http://%s/debug/pprof\n", servingAddr)
	go func() {
		if err := http.Serve(ln, nil); err != nil {
			fmt.Fprintf(os.Stderr, "pprof: serve stopped: %v\n", err)
		}
	}()
	return nil
}

// resolveAddr 把 flag 值归一为监听地址:空/off/none = 关闭,on = 默认本机端口。
func resolveAddr(mode string) string {
	switch mode {
	case "", "off", "none":
		return ""
	case "on":
		return "localhost:6060"
	default:
		return mode
	}
}
