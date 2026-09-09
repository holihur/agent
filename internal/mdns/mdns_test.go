package mdns

import (
	"context"
	"testing"
	"time"
)

// TestAnnounceResolveRoundtrip 验证同主机 Announce ↔ Resolve 回环:
// 应答器注册后,解析器应能发现实例并拿到 IP/Port/TXT。
func TestAnnounceResolveRoundtrip(t *testing.T) {
	stop, err := Announce(context.Background(), "roundtrip-test", 18787, map[string]string{
		"path": "/mcp",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	defer stop()

	// 等应答器进入读循环(主动播报已发出)。
	time.Sleep(1200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svcs, err := Resolve(ctx, 2*time.Second, "roundtrip-test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var found *Service
	for i := range svcs {
		if svcs[i].Instance == "roundtrip-test" {
			found = &svcs[i]
		}
	}
	if found == nil {
		t.Fatalf("instance not found: %+v", svcs)
	}
	if found.Port != 18787 {
		t.Errorf("port = %d, want 18787", found.Port)
	}
	if found.IP == "" {
		t.Error("missing A record")
	}
	if found.Path() != "/mcp" {
		t.Errorf("path = %q, want /mcp", found.Path())
	}
	if got := found.URL(); got[:7] != "http://" {
		t.Errorf("url = %q", got)
	}
}
