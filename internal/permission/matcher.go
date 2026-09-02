package permission

import (
	"encoding/json"

	"github.com/holihur/agent/internal/agent"
)

// wildcardMatch 支持 * 通配任意字符序列（含 /），? 匹配单字符。
// 空 pattern 仅匹配空串。
func wildcardMatch(pattern, s string) bool {
	pi, si := 0, 0
	star := -1
	match := 0
	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == s[si] || pattern[pi] == '?') {
			pi++
			si++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			match = si
			pi++
		} else if star != -1 {
			pi = star + 1
			match++
			si = match
		} else {
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// MatchRule 判断单条规则是否匹配本次工具调用。
// Pattern 为空视为 *，InputPattern 为空视为忽略输入。
func MatchRule(r Rule, call agent.ToolCall) bool {
	pat := r.Pattern
	if pat == "" {
		pat = "*"
	}
	if !wildcardMatch(pat, call.Name) {
		return false
	}
	if r.InputPattern != "" {
		inputStr := string(call.Input)
		// 空输入视作 {}，仍需匹配
		if len(call.Input) == 0 {
			inputStr = ""
		}
		if !wildcardMatch(r.InputPattern, inputStr) {
			// 再尝试对格式化后的 JSON 做宽松匹配（去空格后）
			// 兼容 {"path":"/tmp/a"} 与 {"path": "/tmp/a"} 差异
			var m map[string]any
			if json.Unmarshal(call.Input, &m) == nil {
				if b, err := json.Marshal(m); err == nil {
					if wildcardMatch(r.InputPattern, string(b)) {
						return true
					}
				}
			}
			return false
		}
	}
	return true
}

// IsAllowed 检查调用是否被任一 allow 规则命中。
func (p *Permissions) IsAllowed(call agent.ToolCall) (bool, string) {
	for _, r := range p.Allow {
		if MatchRule(r, call) {
			return true, r.Pattern
		}
	}
	return false, ""
}

// IsDenied 检查调用是否被任一 deny 规则命中。
func (p *Permissions) IsDenied(call agent.ToolCall) (bool, string) {
	for _, r := range p.Deny {
		if MatchRule(r, call) {
			return true, r.Pattern
		}
	}
	return false, ""
}

// IsDeniedOrAllowed 优先判断 deny 再判断 allow，返回 (allowed, denied, pattern)。
func (p *Permissions) IsDeniedOrAllowed(call agent.ToolCall) (bool, bool, string) {
	if ok, pat := p.IsDenied(call); ok {
		return false, true, pat
	}
	if ok, pat := p.IsAllowed(call); ok {
		return true, false, pat
	}
	return false, false, ""
}
