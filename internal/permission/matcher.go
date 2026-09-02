package permission

import (
	"encoding/json"
	"strings"

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
		if len(call.Input) == 0 {
			inputStr = ""
		}
		if !wildcardMatch(r.InputPattern, inputStr) {
			var m map[string]any
			if json.Unmarshal(call.Input, &m) == nil {
				if b, err := json.Marshal(m); err == nil {
					if wildcardMatch(r.InputPattern, string(b)) {
						if isShell(call.Name) {
							if !shellInputAllowsRisky(r.InputPattern, call.Input) {
								return false
							}
						}
						return true
					}
				}
			}
			return false
		}
		if isShell(call.Name) {
			if !shellInputAllowsRisky(r.InputPattern, call.Input) {
				return false
			}
		}
		return true
	}
	return true
}

func isShell(name string) bool { return name == "shell" || name == "local__shell" }

func hasRiskyShellInput(input json.RawMessage) bool {
	cmd := extractCommand(input)
	if cmd == "" {
		return false
	}
	return HasRiskyShellConstruct(cmd)
}

func shellInputAllowsRisky(inputPattern string, input json.RawMessage) bool {
	cmd := extractCommand(input)
	if cmd == "" {
		return true
	}
	if !HasRiskyShellConstruct(cmd) {
		return true
	}
	if HasPipeline(cmd) {
		if !contains(inputPattern, "|") {
			return false
		}
	}
	// 通用风险：串联、后台、子 shell、命令替换等需显式含 ; & $ 或 `
	if HasRiskyShellConstruct(cmd) {
		if !containsAny(inputPattern, []string{"|", ";", "&", "$", "`", "&&", "||"}) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool { return len(sub) > 0 && strings.Contains(s, sub) }
func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if contains(s, sub) {
			return true
		}
	}
	return false
}

func extractCommand(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return string(input)
	}
	if v, ok := m["command"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return string(input)
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
