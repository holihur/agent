package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/mcp"
)

func TestMergeMCPServers(t *testing.T) {
	fromFile := []mcp.JSONServer{
		{Name: "echo", Command: "/tmp/echo-mcp"},
		{Name: "fs", Command: "npx", Args: []string{"-y", "pkg", "/tmp"},
			Env: map[string]string{"B": "2", "A": "1"}},
		{Name: "remote", URL: "https://mcp.example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer tok"}},
	}
	fromFlags := mcpFlags{
		{Name: "fs", Command: "gh-mcp-server"},                // 同名覆盖文件条目
		{Name: "gh", Command: "gh2", Args: []string{"serve"}}, // 新条目追加在后
	}

	specs := mergeMCPServers(fromFile, fromFlags)

	// 覆盖是条目级整体替换:被 flag 覆盖的 fs 丢失文件里的 env,避免混源组合。
	want := []serverSpec{
		{Name: "echo", Command: "/tmp/echo-mcp"},
		{Name: "fs", Command: "gh-mcp-server"},
		{Name: "remote", URL: "https://mcp.example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer tok"}},
		{Name: "gh", Command: "gh2", Args: []string{"serve"}},
	}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("specs = %+v, want %+v", specs, want)
	}
}

func TestMCPFlagsSetURL(t *testing.T) {
	// flag 值以 http(s):// 开头 → 远程服务器,且不接受 args。
	var f mcpFlags
	if err := f.Set("remote=https://mcp.example.com/mcp"); err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 || f[0].URL != "https://mcp.example.com/mcp" || f[0].Command != "" {
		t.Fatalf("f = %+v", f)
	}
	if err := f.Set("bad=https://x/mcp extra"); err == nil {
		t.Fatal("want error for url with args")
	}
	if err := f.Set("plain=http://127.0.0.1:8787/mcp"); err != nil {
		t.Fatal(err)
	}
}

func TestMergeMCPServersEmptyFile(t *testing.T) {
	// 没有 mcp.json 时 flag 独立生效。
	specs := mergeMCPServers(nil, mcpFlags{{Name: "gh", Command: "gh-mcp-server"}})
	if len(specs) != 1 || specs[0].Name != "gh" {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestEnvPairs(t *testing.T) {
	if got := envPairs(nil); got != nil {
		t.Fatalf("nil env = %v, want nil", got)
	}
	got := envPairs(map[string]string{"B": "2", "A": "1"})
	if !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Fatalf("envPairs = %v, want sorted K=V", got)
	}
}

// miniMCPHandler 最小现代 MCP HTTP 服务器:discover + tools/list,足够预检通过。
func miniMCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var fr struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(req.Body).Decode(&fr)
		if fr.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch fr.Method {
		case "server/discover":
			result = map[string]any{"supportedVersions": []string{"2026-07-28"}}
		case "tools/list":
			result = map[string]any{"resultType": "complete", "tools": []map[string]any{}}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": fr.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
}

// TestBuildMCPProvidersSkipsDeadRemote 远程失败 → 警告并跳过,进程继续
// (规范 §mcp.json/remote);死服务器在前也必须不阻断后续健康服务器。
func TestBuildMCPProvidersSkipsDeadRemote(t *testing.T) {
	srv := httptest.NewServer(miniMCPHandler())
	defer srv.Close()

	specs := []serverSpec{
		{Name: "dead", URL: "http://127.0.0.1:1/mcp"},
		{Name: "alive", URL: srv.URL},
	}
	var warns strings.Builder
	providers, err := buildMCPProviders(context.Background(), specs, &warns, nil)
	if err != nil {
		t.Fatalf("err = %v, want dead remote skipped", err)
	}
	if len(providers) != 1 || providers[0].Namespace() != "alive" {
		t.Fatalf("providers = %+v", providers)
	}
	if !strings.Contains(warns.String(), `skipping remote server "dead"`) {
		t.Fatalf("warns = %q", warns.String())
	}
	for _, p := range providers {
		_ = p.Close()
	}
}

// TestBuildMCPProvidersFailsFastOnStdio stdio 失败(命令不存在)→ fail-fast,不跳过。
func TestBuildMCPProvidersFailsFastOnStdio(t *testing.T) {
	srv := httptest.NewServer(miniMCPHandler())
	defer srv.Close()

	specs := []serverSpec{
		{Name: "alive", URL: srv.URL},
		{Name: "broken", Command: "/nonexistent-cmd-xyz"},
	}
	providers, err := buildMCPProviders(context.Background(), specs, &strings.Builder{}, nil)
	if err == nil {
		t.Fatal("want error for missing stdio command")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err = %v, want broken in message", err)
	}
	for _, p := range providers {
		_ = p.Close()
	}
}
