package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// JSONServer 是 mcp.json 中单个服务器的条目(规范 §mcp.json/format)。
// Command(URL)与 URL(HTTP)二选一:前者是 stdio 子进程,后者是 Streamable HTTP 远程。
type JSONServer struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string // 仅 stdio
	URL     string            // 仅 http:必须 http:// 或 https:// 开头
	Headers map[string]string // 仅 http:附加请求头(如 Authorization)
}

// jsonServerNameRe 与 tools 层命名空间校验保持一致(规范 §mcp.json/validation)。
var jsonServerNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// LoadJSONConfig 按规范 §mcp.json/discovery 依次探测 paths:
// 第一个存在的文件生效,其余忽略;全部不存在 → (nil, nil)。
// 找到的文件解析或校验失败 → fail-fast,返回含文件名的明确错误。
func LoadJSONConfig(paths ...string) ([]JSONServer, error) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("mcp: read %s: %w", path, err)
		}
		servers, err := parseJSONConfig(data, path)
		if err != nil {
			return nil, err
		}
		return servers, nil
	}
	return nil, nil
}

// parseJSONConfig 解析并校验 mcp.json 内容。
// 用 Token 流式走读以保留 mcpServers 的文档顺序(规范 §mcp.json/merge);
// 未知顶层键与服务器级未知字段容忍并忽略(规范 §mcp.json/format 互操作)。
func parseJSONConfig(data []byte, path string) ([]JSONServer, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("mcp: %s: %s", path, fmt.Sprintf(format, args...))
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, fail("invalid JSON: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fail("top level must be an object")
	}

	var servers []JSONServer
	found := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fail("invalid JSON: %v", err)
		}
		if key := keyTok.(string); key != "mcpServers" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fail("invalid JSON: %v", err)
			}
			continue
		}
		found = true
		tok, err := dec.Token()
		if err != nil {
			return nil, fail("invalid JSON: %v", err)
		}
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			return nil, fail(`"mcpServers" must be an object`)
		}
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return nil, fail("invalid JSON: %v", err)
			}
			name := nameTok.(string)
			var entry jsonServerEntry
			if err := dec.Decode(&entry); err != nil {
				return nil, fail("server %q: %v", name, err)
			}
			if err := validateJSONServer(name, entry); err != nil {
				return nil, fail("%v", err)
			}
			for _, s := range servers {
				if s.Name == name {
					return nil, fail("duplicate server name %q", name)
				}
			}
			servers = append(servers, JSONServer{
				Name: name, Command: entry.Command, Args: entry.Args, Env: entry.Env,
				URL: entry.URL, Headers: entry.Headers,
			})
		}
		if _, err := dec.Token(); err != nil { // mcpServers 闭括号
			return nil, fail("invalid JSON: %v", err)
		}
	}
	if !found {
		return nil, fail(`missing "mcpServers" object`)
	}
	return servers, nil
}

// jsonServerEntry 是 mcp.json 中单个服务器的原始解码形状。
type jsonServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// validateJSONServer 实施规范 §mcp.json/validation:
//   - 名称 ^[a-zA-Z0-9_-]+$
//   - command(stdio)与 url(http)恰好一个;url 必须 http(s):// 开头
//   - env 仅 stdio,headers 仅 http(错放即报错,防配置写错传输类型)
func validateJSONServer(name string, e jsonServerEntry) error {
	if !jsonServerNameRe.MatchString(name) {
		return fmt.Errorf("server %q: name must match ^[a-zA-Z0-9_-]+$", name)
	}
	switch {
	case e.Command != "" && e.URL != "":
		return fmt.Errorf("server %q: command and url are mutually exclusive", name)
	case e.Command != "": // stdio
		if e.Headers != nil {
			return fmt.Errorf("server %q: headers only apply to http servers", name)
		}
		return nil
	case e.URL != "": // http
		if !strings.HasPrefix(e.URL, "http://") && !strings.HasPrefix(e.URL, "https://") {
			return fmt.Errorf("server %q: url must start with http:// or https://, got %q", name, e.URL)
		}
		if e.Env != nil {
			return fmt.Errorf("server %q: env only applies to stdio servers", name)
		}
		return nil
	default:
		return fmt.Errorf("server %q: missing command (stdio) or url (http)", name)
	}
}
