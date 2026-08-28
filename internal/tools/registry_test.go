package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---- 测试替身 ----

type fakeProv struct {
	ns    string
	tools []ToolDef
	err   error

	gotName  string
	gotInput json.RawMessage
	result   ToolResult
	callErr  error
}

func (f *fakeProv) Namespace() string { return f.ns }

func (f *fakeProv) ListTools(_ context.Context) ([]ToolDef, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tools, nil
}

func (f *fakeProv) CallTool(_ context.Context, name string, input json.RawMessage) (ToolResult, error) {
	f.gotName = name
	f.gotInput = append(json.RawMessage(nil), input...)
	if f.callErr != nil {
		return ToolResult{}, f.callErr
	}
	return f.result, nil
}

// ---- Registry ----

func TestRegistryNamespaceProjection(t *testing.T) {
	reg := New()
	if err := reg.Register(&fakeProv{ns: "local", tools: []ToolDef{{Name: "shell", Description: "c"}}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&fakeProv{ns: "fs", tools: []ToolDef{{Name: "read_file", Description: "r"}}}); err != nil {
		t.Fatal(err)
	}
	defs, err := reg.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["shell"] { // local 不加前缀
		t.Errorf("local tool not exposed unprefixed: %v", names)
	}
	if !names["fs__read_file"] { // mcp 加命名空间前缀
		t.Errorf("mcp tool not namespaced: %v", names)
	}
}

func TestRegistryRoutesByExposedName(t *testing.T) {
	p := &fakeProv{ns: "fs", tools: []ToolDef{{Name: "read_file", Description: "r"}}, result: ToolResult{Text: "contents"}}
	reg := New()
	if err := reg.Register(p); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Call(context.Background(), "fs__read_file", json.RawMessage(`{"path":"/tmp/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "contents" {
		t.Fatalf("result = %+v", res)
	}
	if p.gotName != "read_file" { // 路由到原始名,而非暴露名
		t.Fatalf("provider got %q", p.gotName)
	}
	if string(p.gotInput) != `{"path":"/tmp/x"}` {
		t.Fatalf("input = %s", p.gotInput)
	}
}

func TestRegistryDuplicateNamespace(t *testing.T) {
	reg := New()
	if err := reg.Register(&fakeProv{ns: "fs"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&fakeProv{ns: "fs"}); err == nil {
		t.Fatal("expected duplicate namespace error")
	}
}

func TestRegistryInvalidExposedName(t *testing.T) {
	// MCP 允许点号工具名,Anthropic 不允许 → 暴露名校验必须拦截。
	reg := New()
	if err := reg.Register(&fakeProv{ns: "fs", tools: []ToolDef{{Name: "read.file", Description: "r"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Tools(context.Background()); err == nil {
		t.Fatal("expected invalid exposed name error")
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	reg := New()
	if err := reg.Register(&fakeProv{ns: "local", tools: []ToolDef{{Name: "a", Description: "d"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Call(context.Background(), "nope", nil); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

func TestRegistryProviderFailure(t *testing.T) {
	reg := New()
	if err := reg.Register(&fakeProv{ns: "fs", err: errors.New("server down")}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Tools(context.Background()); err == nil {
		t.Fatal("expected provider failure to surface")
	}
}

// ---- builtin:shell ----

func callBuiltin(t *testing.T, input string) (ToolResult, error) {
	t.Helper()
	p, err := NewBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	return p.CallTool(context.Background(), "shell", json.RawMessage(input))
}

func TestShellCapturesStreamsAndExitCode(t *testing.T) {
	res, err := callBuiltin(t, `{"command":"echo out; echo err 1>&2; exit 3"}`)
	if err != nil {
		t.Fatal(err) // 非零退出码不是 Go error
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %+v", res)
	}
	for _, want := range []string{"exit: 3", "--- stdout ---", "out", "--- stderr ---", "err"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("result %q missing %q", res.Text, want)
		}
	}
}

func TestShellZeroExit(t *testing.T) {
	res, err := callBuiltin(t, `{"command":"echo hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "exit: 0") || !strings.Contains(res.Text, "hello") {
		t.Fatalf("result = %q", res.Text)
	}
}

func TestShellOutputTruncation(t *testing.T) {
	res, err := callBuiltin(t, `{"command":"yes | head -c 200000"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "[truncated") {
		t.Fatalf("expected truncation notice, got len=%d", len(res.Text))
	}
}

func TestShellTimeout(t *testing.T) {
	old := shellTimeout
	shellTimeout = 150 * time.Millisecond
	defer func() { shellTimeout = old }()

	if _, err := callBuiltin(t, `{"command":"sleep 5"}`); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error", err)
	}
}

func TestShellArgumentValidation(t *testing.T) {
	if _, err := callBuiltin(t, `{}`); err == nil {
		t.Fatal("expected missing command error")
	}
	if _, err := callBuiltin(t, `not json`); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestBuiltinSingleTool(t *testing.T) {
	p, err := NewBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	defs, err := p.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "shell" {
		t.Fatalf("builtins = %+v, want only shell", defs)
	}
}
