package permission

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/holihur/agent/internal/agent"
)

// version 是持久化格式版本，未来扩展时做迁移。
const version = 1

// Rule 是一条持久化规则。
type Rule struct {
	Pattern      string `json:"pattern"`                // 工具名通配，如 "fs__*" / "*" / "shell__run"
	InputPattern string `json:"inputPattern,omitempty"` // 可选，输入 JSON 的通配，如 "*\"/tmp/*\"*"
	AddedAt      string `json:"addedAt,omitempty"`
	Source       string `json:"source,omitempty"` // 写入来源 cwd 或 global
}

// Permissions 是文件落盘结构。
type Permissions struct {
	Version int    `json:"version"`
	Allow   []Rule `json:"allow"`
	Deny    []Rule `json:"deny"`
	mu      sync.RWMutex `json:"-"`
	path    string       `json:"-"`
}

// ProjectPath 返回项目级权限文件路径。
func ProjectPath(cwd string) string {
	if cwd == "" {
		cwd = "."
	}
	return filepath.Join(cwd, ".agent", "permissions.json")
}

// GlobalPath 返回全局权限文件路径 ~/.config/agent/permissions.json。
func GlobalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "agent", "permissions.json"), nil
}

// newEmpty 创建空权限集。
func newEmpty() *Permissions {
	return &Permissions{Version: version}
}

// loadFile 读取单个文件，不存在返回空集。
func loadFile(path string) (*Permissions, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		p := newEmpty()
		p.path = path
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("permission: read %s: %w", path, err)
	}
	if len(data) == 0 {
		p := newEmpty()
		p.path = path
		return p, nil
	}
	var p Permissions
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("permission: parse %s: %w", path, err)
	}
	if p.Version == 0 {
		p.Version = version
	}
	if p.Allow == nil {
		p.Allow = []Rule{}
	}
	if p.Deny == nil {
		p.Deny = []Rule{}
	}
	p.path = path
	return &p, nil
}

// saveFile 原子写入文件，目录自动创建，权限 0600。
func saveFile(path string, p *Permissions) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("permission: mkdir %s: %w", dir, err)
	}
	p.Version = version
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("permission: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("permission: rename %s: %w", tmp, err)
	}
	return nil
}

// Load 合并全局与项目级权限，项目级追加在后（去重后项目级优先展示）。
// cwd 为项目根，未提供则用当前工作目录。
func Load(cwd string) (*Permissions, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "."
		}
	}
	globalPath, err := GlobalPath()
	if err != nil {
		globalPath = ""
	}
	var merged = newEmpty()
	// 全局
	if globalPath != "" {
		gp, err := loadFile(globalPath)
		if err != nil {
			return nil, err
		}
		merged.Allow = append(merged.Allow, gp.Allow...)
		merged.Deny = append(merged.Deny, gp.Deny...)
	}
	// 项目
	pp, err := loadFile(ProjectPath(cwd))
	if err != nil {
		return nil, err
	}
	merged.Allow = append(merged.Allow, pp.Allow...)
	merged.Deny = append(merged.Deny, pp.Deny...)
	merged = dedup(merged)
	// 记录主路径为项目路径，后续 Add/Remove 默认写项目级
	merged.path = ProjectPath(cwd)
	return merged, nil
}

// LoadProject 仅加载项目级。
func LoadProject(cwd string) (*Permissions, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "."
		}
	}
	return loadFile(ProjectPath(cwd))
}

// dedup 去重（pattern+inputPattern 为键），保留最后一次出现。
func dedup(p *Permissions) *Permissions {
	seen := make(map[string]bool)
	var outAllow []Rule
	for i := len(p.Allow) - 1; i >= 0; i-- {
		r := p.Allow[i]
		key := r.Pattern + "\x00" + r.InputPattern
		if seen[key] {
			continue
		}
		seen[key] = true
		outAllow = append(outAllow, r)
	}
	// 反转回原序
	for i, j := 0, len(outAllow)-1; i < j; i, j = i+1, j-1 {
		outAllow[i], outAllow[j] = outAllow[j], outAllow[i]
	}
	seen = make(map[string]bool)
	var outDeny []Rule
	for i := len(p.Deny) - 1; i >= 0; i-- {
		r := p.Deny[i]
		key := r.Pattern + "\x00" + r.InputPattern
		if seen[key] {
			continue
		}
		seen[key] = true
		outDeny = append(outDeny, r)
	}
	for i, j := 0, len(outDeny)-1; i < j; i, j = i+1, j-1 {
		outDeny[i], outDeny[j] = outDeny[j], outDeny[i]
	}
	p.Allow = outAllow
	p.Deny = outDeny
	if p.Allow == nil {
		p.Allow = []Rule{}
	}
	if p.Deny == nil {
		p.Deny = []Rule{}
	}
	return p
}

// saveProject 持久化到项目文件。
func saveProject(cwd string, p *Permissions) error {
	return saveFile(ProjectPath(cwd), p)
}

// saveGlobal 持久化到全局文件。
func saveGlobal(p *Permissions) error {
	gp, err := GlobalPath()
	if err != nil {
		return err
	}
	return saveFile(gp, p)
}

// Deny 添加一条 deny 规则到项目级（去重），global 控制是否写全局。
func Deny(cwd, pattern, inputPattern string, global bool) error {
	if pattern == "" {
		return errors.New("permission: pattern required")
	}
	var p *Permissions
	var err error
	if global {
		gp, e := GlobalPath()
		if e != nil {
			return e
		}
		p, err = loadFile(gp)
		if err != nil {
			return err
		}
		for _, r := range p.Deny {
			if r.Pattern == pattern && r.InputPattern == inputPattern {
				return nil
			}
		}
		p.Deny = append(p.Deny, Rule{Pattern: pattern, InputPattern: inputPattern, AddedAt: time.Now().Format(time.RFC3339)})
		return saveGlobal(p)
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	p, err = loadFile(ProjectPath(cwd))
	if err != nil {
		return err
	}
	for _, r := range p.Deny {
		if r.Pattern == pattern && r.InputPattern == inputPattern {
			return nil
		}
	}
	p.Deny = append(p.Deny, Rule{Pattern: pattern, InputPattern: inputPattern, AddedAt: time.Now().Format(time.RFC3339)})
	return saveProject(cwd, p)
}

// Allow 添加一条 allow 规则到项目级（去重），global 控制是否写全局。
func Allow(cwd, pattern, inputPattern string, global bool) error {
	if pattern == "" {
		return errors.New("permission: pattern required")
	}
	var p *Permissions
	var err error
	if global {
		gp, e := GlobalPath()
		if e != nil {
			return e
		}
		p, err = loadFile(gp)
		if err != nil {
			return err
		}
		// 去重
		for _, r := range p.Allow {
			if r.Pattern == pattern && r.InputPattern == inputPattern {
				return nil
			}
		}
		p.Allow = append(p.Allow, Rule{Pattern: pattern, InputPattern: inputPattern, AddedAt: time.Now().Format(time.RFC3339)})
		return saveGlobal(p)
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	p, err = loadFile(ProjectPath(cwd))
	if err != nil {
		return err
	}
	for _, r := range p.Allow {
		if r.Pattern == pattern && r.InputPattern == inputPattern {
			return nil
		}
	}
	p.Allow = append(p.Allow, Rule{Pattern: pattern, InputPattern: inputPattern, AddedAt: time.Now().Format(time.RFC3339)})
	return saveProject(cwd, p)
}

// Revoke 移除一条规则（同时匹配 allow 与 deny），global 控制范围。
func Revoke(cwd, pattern string, global bool) error {
	if pattern == "" {
		return errors.New("permission: pattern required")
	}
	filter := func(rules []Rule) []Rule {
		out := []Rule{}
		for _, r := range rules {
			if r.Pattern == pattern {
				continue
			}
			// 支持通配删除：若 pattern 含 * 则按 glob 匹配删除
			if containsWildcard(pattern) && wildcardMatch(pattern, r.Pattern) {
				continue
			}
			out = append(out, r)
		}
		return out
	}
	if global {
		gp, err := GlobalPath()
		if err != nil {
			return err
		}
		p, err := loadFile(gp)
		if err != nil {
			return err
		}
		before := len(p.Allow) + len(p.Deny)
		p.Allow = filter(p.Allow)
		p.Deny = filter(p.Deny)
		if len(p.Allow)+len(p.Deny) == before {
			return fmt.Errorf("permission: pattern %q not found in global", pattern)
		}
		return saveGlobal(p)
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	p, err := loadFile(ProjectPath(cwd))
	if err != nil {
		return err
	}
	before := len(p.Allow) + len(p.Deny)
	p.Allow = filter(p.Allow)
	p.Deny = filter(p.Deny)
	if len(p.Allow)+len(p.Deny) == before {
		return fmt.Errorf("permission: pattern %q not found", pattern)
	}
	return saveProject(cwd, p)
}

// List 返回合并后的视图（用于展示）。
func List(cwd string) (*Permissions, error) {
	return Load(cwd)
}

// Check 检查调用是否被持久化规则命中，返回是否允许及命中 pattern。
func Check(cwd string, call agent.ToolCall) (bool, string, error) {
	p, err := Load(cwd)
	if err != nil {
		return false, "", err
	}
	ok, pat := p.IsAllowed(call)
	return ok, pat, nil
}

// CheckWithDeny 检查并区分 deny 与 allow，返回 (allowed, denied, pattern)。
func CheckWithDeny(cwd string, call agent.ToolCall) (bool, bool, string, error) {
	p, err := Load(cwd)
	if err != nil {
		return false, false, "", err
	}
	allowed, denied, pat := p.IsDeniedOrAllowed(call)
	return allowed, denied, pat, nil
}

// CheckAllowed 是 Check 的简化版，忽略错误时视为未命中（不阻断执行）。
func CheckAllowed(cwd string, call agent.ToolCall) bool {
	ok, _, _ := Check(cwd, call)
	return ok
}

func containsWildcard(s string) bool {
	for _, ch := range s {
		if ch == '*' || ch == '?' {
			return true
		}
	}
	return false
}
