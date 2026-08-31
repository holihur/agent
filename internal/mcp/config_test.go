package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp 把内容写入临时目录下的 name,返回完整路径。
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---- discovery(规范 §mcp.json/discovery)----

func TestLoadJSONConfigAllMissing(t *testing.T) {
	// 所有候选都不存在 → 静默无配置,不是错误。
	servers, err := LoadJSONConfig(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if servers != nil {
		t.Fatalf("servers = %+v, want nil", servers)
	}
}

func TestLoadJSONConfigFirstExistingWins(t *testing.T) {
	// 第一个存在的文件生效,其余忽略:even 第二个文件语法错误也不看。
	good := writeTemp(t, "mcp.json", `{"mcpServers":{"a":{"command":"x"}}}`)
	bad := writeTemp(t, "other.json", "not json")
	servers, err := LoadJSONConfig(good, bad)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "a" {
		t.Fatalf("servers = %+v", servers)
	}

	// 第一个不存在时探测下一个。
	missing := filepath.Join(t.TempDir(), "absent.json")
	servers, err = LoadJSONConfig(missing, good)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "a" {
		t.Fatalf("servers = %+v", servers)
	}
}

// ---- format(规范 §mcp.json/format)----

func TestParsePreservesDocumentOrder(t *testing.T) {
	// 注册顺序 = 文档顺序(规范 §mcp.json/merge),map 反序列化做不到,必须流式走读。
	data := `{"mcpServers":{
		"echo":{"command":"/tmp/echo-mcp"},
		"fs":{"command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","/tmp"]},
		"gh":{"command":"gh-mcp-server","env":{"GH_TOKEN":"x"}}
	}}`
	servers, err := parseJSONConfig([]byte(data), "mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range servers {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "echo,fs,gh" {
		t.Fatalf("order = %v", names)
	}
	if servers[1].Args[len(servers[1].Args)-1] != "/tmp" {
		t.Fatalf("fs args = %+v", servers[1].Args)
	}
	if servers[2].Env["GH_TOKEN"] != "x" {
		t.Fatalf("gh env = %+v", servers[2].Env)
	}
}

func TestParseToleratesUnknownFields(t *testing.T) {
	// 互操作:Claude Code / Cursor 等文件常带 type、cwd 等字段,必须容忍并忽略。
	data := `{
		"$schema": "https://example.com/schema.json",
		"mcpServers":{
			"fs":{"type":"stdio","command":"npx","args":["-y","pkg"],"cwd":"/tmp","env":{"K":"V"}}
		}
	}`
	servers, err := parseJSONConfig([]byte(data), "mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Command != "npx" || servers[0].Env["K"] != "V" {
		t.Fatalf("servers = %+v", servers)
	}
}

func TestParseEmptyServersObject(t *testing.T) {
	servers, err := parseJSONConfig([]byte(`{"mcpServers":{}}`), "mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("servers = %+v, want empty", servers)
	}
}

// ---- validation(规范 §mcp.json/validation)----

func TestParseRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"malformed json", `{"mcpServers":`, "invalid JSON"},
		{"top level array", `[]`, "top level must be an object"},
		{"missing mcpServers", `{"other":{}}`, `missing "mcpServers"`},
		{"mcpServers not object", `{"mcpServers":[]}`, `"mcpServers" must be an object`},
		{"bad name", `{"mcpServers":{"a b":{"command":"x"}}}`, "name must match"},
		{"missing both command and url", `{"mcpServers":{"a":{}}}`, "missing command (stdio) or url (http)"},
		{"empty command", `{"mcpServers":{"a":{"command":""}}}`, "missing command (stdio) or url (http)"},
		{"non string env", `{"mcpServers":{"a":{"command":"x","env":{"K":1}}}}`, "server \"a\""},
		{"duplicate name", `{"mcpServers":{"a":{"command":"x"},"a":{"command":"y"}}}`, "duplicate server name"},
		{"command and url exclusive", `{"mcpServers":{"a":{"command":"x","url":"http://h/mcp"}}}`, "mutually exclusive"},
		{"url bad scheme", `{"mcpServers":{"a":{"url":"ftp://h/mcp"}}}`, "url must start with"},
		{"env on http server", `{"mcpServers":{"a":{"url":"http://h/mcp","env":{"K":"V"}}}}`, "env only applies to stdio"},
		{"headers on stdio server", `{"mcpServers":{"a":{"command":"x","headers":{"K":"V"}}}}`, "headers only apply to http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseJSONConfig([]byte(tc.data), "mcp.json")
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			// 错误必须带文件名,便于定位(规范 §mcp.json/errors)。
			if !strings.Contains(err.Error(), "mcp.json") {
				t.Fatalf("err = %v, want file path in message", err)
			}
		})
	}
}

// ---- 远程服务器(规范 §mcp.json/remote)----

func TestParseHTTPServer(t *testing.T) {
	data := `{"mcpServers":{
		"remote":{"url":"https://mcp.example.com/mcp","headers":{"Authorization":"Bearer tok"}}
	}}`
	servers, err := parseJSONConfig([]byte(data), "mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %+v", servers)
	}
	s := servers[0]
	if s.URL != "https://mcp.example.com/mcp" || s.Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("server = %+v", s)
	}
	if s.Command != "" || s.Env != nil {
		t.Fatalf("stdio fields must stay empty on http servers: %+v", s)
	}
}
