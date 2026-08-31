// Package skills 实现启动时扫描技能目录(默认 cwd 下 .agents/skills/)的 hook:
// 每个直接子目录若含 SKILL.md 即一个技能(frontmatter 的 name/description 可选,
// name 缺省回落目录名)。技能清单并入 system prompt,并注册 skill 工具供模型
// 按需加载单个技能的完整指令——清单常驻、正文懒加载,避免 prompt 膨胀。
package skills

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/tools"
)

// defaultSkillsDir 是 -skills 的默认扫描目录(相对 cwd)。
const defaultSkillsDir = ".agents/skills"

// skillFile 是技能包内的约定文件名(https://agentskills.io 形状的最小子集)。
const skillFile = "SKILL.md"

var skillsDir = flag.String("skills", defaultSkillsDir,
	"skills directory (relative to cwd or absolute); off/none disables (hook)")

func init() {
	hook.Register("skills", installSkills)
}

// skill 是一个已发现的技能包。
type skill struct {
	Name        string // 暴露名:frontmatter name,缺省用目录名
	Description string // 一句话说明(清单展示用,可空)
	Body        string // SKILL.md 正文(去掉 frontmatter),skill 工具按需返回
}

// installSkills 启动时扫描一次技能目录:有技能时注册 skill 工具并把清单
// 挂到每轮 Turn 的 system prompt(会话内保持一致,不随磁盘变化)。
func installSkills(h *agent.Hooks, d hook.Deps) error {
	mode := *skillsDir
	if mode == "off" || mode == "none" {
		return nil
	}
	dir := mode
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(d.CWD, dir)
	}
	found, err := discoverSkills(dir)
	if err != nil || len(found) == 0 {
		return err
	}
	if d.Tools == nil {
		return errors.New("skills hook needs the local tool provider (Deps.Tools)")
	}
	if err := registerSkillTool(d.Tools, found); err != nil {
		return err
	}
	h.OnMutateTurnRequest(func(r agent.TurnRequest) agent.TurnRequest {
		r.System = mergeSystem(r.System, catalog(found))
		return r
	})
	names := make([]string, 0, len(found))
	for _, s := range found {
		names = append(names, s.Name)
	}
	fmt.Fprintf(os.Stderr, "skills: %s (%s)\n", strings.Join(names, ", "), dir)
	return nil
}

// discoverSkills 扫描技能根目录:每个直接子目录若含 SKILL.md 即一个技能。
// 目录不存在 → 静默返回空(多数项目没有技能是常态);读取/解析失败 → fail-fast。
// frontmatter name 缺省回落目录名;重名立即报错;按名字排序保证确定性。
func discoverSkills(root string) ([]skill, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills dir %s: %w", root, err)
	}
	var out []skill
	seen := map[string]string{} // 暴露名 → 来源目录(重名报错用)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), skillFile)
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // 无 SKILL.md 的目录不是技能包,静默跳过
			}
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		meta, body := splitFrontmatter(string(data))
		name := meta["name"]
		if name == "" {
			name = e.Name()
		}
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("skills: duplicate skill name %q (%s and %s)", name, prev, filepath.Join(root, e.Name()))
		}
		seen[name] = filepath.Join(root, e.Name())
		out = append(out, skill{Name: name, Description: meta["description"], Body: body})
	}
	slices.SortFunc(out, func(a, b skill) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// splitFrontmatter 解析 SKILL.md 的可选 YAML frontmatter(仅逐行 key: value
// 的最小子集,值支持成对引号)+ 正文。无 frontmatter(或未闭合)时整篇即正文。
func splitFrontmatter(text string) (meta map[string]string, body string) {
	lines := strings.Split(text, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "---" {
		return nil, strings.TrimSpace(text)
	}
	i++
	meta = map[string]string{}
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		i++
		if line == "---" {
			return meta, strings.TrimSpace(strings.Join(lines[i:], "\n"))
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		meta[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return nil, strings.TrimSpace(text) // 未闭合:宽松处理,整篇视为正文
}

// unquote 去掉值两侧成对的单/双引号。
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == v[len(v)-1]) && (v[0] == '"' || v[0] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// registerSkillTool 在进程内工具平面上注册 skill 工具:按名返回单个技能的正文。
func registerSkillTool(p *tools.LocalProvider, found []skill) error {
	byName := make(map[string]string, len(found))
	names := make([]string, 0, len(found))
	for _, s := range found {
		byName[s.Name] = s.Body
		names = append(names, s.Name)
	}
	return p.Register(tools.ToolDef{
		Name: "skill",
		Description: "Load the full instructions of an available skill by name. " +
			"Call it with a skill's name (from the Available Skills list in the system prompt) " +
			"before doing work that matches that skill.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Skill name, exactly as listed in Available Skills",
				},
			},
			"required": []string{"name"},
		},
	}, func(_ context.Context, input json.RawMessage) (string, error) {
		var args struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if args.Name == "" {
			return "", fmt.Errorf(`missing required argument "name"`)
		}
		body, ok := byName[args.Name]
		if !ok {
			return "", fmt.Errorf("unknown skill %q (available: %s)", args.Name, strings.Join(names, ", "))
		}
		if strings.TrimSpace(body) == "" {
			return fmt.Sprintf("skill %q has no instructions body", args.Name), nil
		}
		return body, nil
	})
}

// catalog 把技能清单渲染为注入 system prompt 的一节。
func catalog(found []skill) string {
	var b strings.Builder
	b.WriteString("# Available Skills\n\n")
	b.WriteString("When a task matches one of the skills below, call the `skill` tool with that skill's name " +
		"first to load its full instructions, then follow them.")
	for _, s := range found {
		b.WriteString("\n- " + s.Name)
		if s.Description != "" {
			b.WriteString(": " + s.Description)
		}
	}
	return b.String()
}

// mergeSystem 把技能清单并入 system prompt:基础 prompt 在前,清单在后。
func mergeSystem(base, extra string) string {
	switch {
	case strings.TrimSpace(extra) == "":
		return base
	case strings.TrimSpace(base) == "":
		return extra
	default:
		return strings.TrimRight(base, "\n") + "\n\n" + extra
	}
}
