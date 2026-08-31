package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// LocalProvider 是进程内函数工具的 Provider(namespace 固定 "local",暴露名不加前缀)。
type LocalProvider struct {
	tools map[string]localTool
}

type localTool struct {
	def ToolDef
	fn  func(ctx context.Context, input json.RawMessage) (string, error)
}

func NewLocal() *LocalProvider {
	return &LocalProvider{tools: map[string]localTool{}}
}

func (p *LocalProvider) Namespace() string { return localNamespace }

// Register 登记一个进程内工具。
func (p *LocalProvider) Register(def ToolDef, fn func(ctx context.Context, input json.RawMessage) (string, error)) error {
	if !namePattern.MatchString(def.Name) {
		return fmt.Errorf("tools: invalid tool name %q", def.Name)
	}
	if _, dup := p.tools[def.Name]; dup {
		return fmt.Errorf("tools: duplicate local tool %q", def.Name)
	}
	p.tools[def.Name] = localTool{def: def, fn: fn}
	return nil
}

func (p *LocalProvider) ListTools(_ context.Context) ([]ToolDef, error) {
	out := make([]ToolDef, 0, len(p.tools))
	for _, t := range p.tools {
		out = append(out, t.def)
	}
	// 确定性顺序(MCP 规范建议,利于 LLM prompt 缓存)。
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p *LocalProvider) CallTool(ctx context.Context, name string, input json.RawMessage) (ToolResult, error) {
	t, ok := p.tools[name]
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown local tool %q", name)
	}
	text, err := t.fn(ctx, input)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Text: text}, nil
}

// ---- 内置工具:shell + 文件工具(read/write/edit) ----
//
// 以 agent 进程权限运行,无沙箱,面向本地个人使用。
// git/grep/构建/列目录 经 shell 命令完成;
// 读文件/写文件/改文件 优先用 read/write/edit(三者均支持批量)。

const maxShellOutput = 64 << 10 // 输出上限(stdout/stderr 各自截断)

// shellTimeout 是命令超时(var 以便测试覆盖)。
var shellTimeout = 30 * time.Second

// NewBuiltin 返回预置工具的 LocalProvider:shell + 文件工具(read/write/edit,均支持批量)。
func NewBuiltin() (*LocalProvider, error) {
	p := NewLocal()
	if err := RegisterShell(p); err != nil {
		return nil, err
	}
	if err := RegisterFS(p); err != nil {
		return nil, err
	}
	return p, nil
}

// RegisterShell 把内置 shell 工具注册到给定的进程内工具平面。
// 供 NewBuiltin 与嵌入式门面(agent.Shell)共享同一实现。
func RegisterShell(p *LocalProvider) error {
	return p.Register(ToolDef{
		Name:        "shell",
		Description: fmt.Sprintf("Runs a shell command via `sh -c` with a %s timeout. Non-zero exit codes are reported in the result (not treated as tool failure). Use it for git, grep, builds, listing directories, etc. Prefer the read/write/edit tools for reading and modifying files.", shellTimeout),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command to run",
				},
			},
			"required": []string{"command"},
		},
	}, toolShell)
}

func toolShell(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Command == "" {
		return "", fmt.Errorf(`missing required argument "command"`)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("command timed out after %s", shellTimeout)
	}

	// 非零退出码是"结果"而非工具失败 —— 模型可以直接利用退出码与输出自我修正。
	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return "", fmt.Errorf("failed to run: %w", runErr)
		}
		exitCode = ee.ExitCode()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit: %d", exitCode)
	if out := truncateOutput(stdout.String(), maxShellOutput); out != "" {
		b.WriteString("\n--- stdout ---\n")
		b.WriteString(out)
	}
	if errMsg := truncateOutput(stderr.String(), maxShellOutput); errMsg != "" {
		b.WriteString("\n--- stderr ---\n")
		b.WriteString(errMsg)
	}
	return b.String(), nil
}

func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n[truncated: showing first %d of %d bytes]", max, len(s))
}
