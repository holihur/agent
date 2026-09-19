package mcpserver

import "testing"

// TestLoopbackAddr 覆盖安全评审 §L10：区分回环与对外监听地址。
func TestLoopbackAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1:8788", "localhost:8788", "[::1]:8788"} {
		if !loopbackAddr(a) {
			t.Fatalf("loopbackAddr(%q) = false", a)
		}
	}
	for _, a := range []string{":8788", "0.0.0.0:8788", "10.0.0.1:8788"} {
		if loopbackAddr(a) {
			t.Fatalf("loopbackAddr(%q) = true", a)
		}
	}
}
