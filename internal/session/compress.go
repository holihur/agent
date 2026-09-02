package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/holihur/agent/internal/agent"
)

// Compressor 是会话压缩算法的接口，便于切换不同实现。
// 例如：SimpleCompressor（截断占位）、LLMCompressor（调用模型总结）。
type Compressor interface {
	Name() string
	Compress(ctx context.Context, msgs []agent.Message) (string, error)
}

// SimpleCompressor 仅做占位截断，返回固定格式摘要，不依赖 LLM。
type SimpleCompressor struct{}

func (SimpleCompressor) Name() string { return "simple" }

func (SimpleCompressor) Compress(_ context.Context, msgs []agent.Message) (string, error) {
	if len(msgs) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[compressed %d messages]\n", len(msgs)))
	// 提取每条消息的首行文本作为摘要，避免过长
	limit := 20
	if len(msgs) < limit {
		limit = len(msgs)
	}
	for i := 0; i < limit; i++ {
		m := msgs[i]
		txt := messagePreview(m)
		if txt != "" {
			fmt.Fprintf(&b, "- %s: %s\n", m.Role, truncate(txt, 120))
		}
	}
	if len(msgs) > limit {
		fmt.Fprintf(&b, "... and %d more messages\n", len(msgs)-limit)
	}
	return b.String(), nil
}

// LLMCompressor 通过 LLM 总结历史，需外部注入 SummarizeFn。
type LLMCompressor struct {
	Summarize func(ctx context.Context, prompt string) (string, error)
}

func (c LLMCompressor) Name() string { return "llm" }

func (c LLMCompressor) Compress(ctx context.Context, msgs []agent.Message) (string, error) {
	if c.Summarize == nil {
		return SimpleCompressor{}.Compress(ctx, msgs)
	}
	var b strings.Builder
	b.WriteString("Summarize the following conversation history concisely, preserving key decisions, tool results, and user intent:\n\n")
	for _, m := range msgs {
		txt := messagePreview(m)
		if txt == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, txt)
	}
	return c.Summarize(ctx, b.String())
}

// Config 是压缩触发的配置。
type Config struct {
	MaxTokens  int       // 阈值，如 100000
	Ratio      float64   // 触发比例，如 0.8 表示 80%
	KeepRecent int       // 压缩后保留的最近消息数（轮次*2），如 6 表示最后 3 轮
	Compressor Compressor
}

// DefaultConfig 返回默认配置：100w 80% 触发，保留最后 6 条消息。
func DefaultConfig() Config {
	return Config{
		MaxTokens:  100000,
		Ratio:      0.8,
		KeepRecent: 6,
		Compressor: SimpleCompressor{},
	}
}

// Validate 校验配置合法性。
func (c Config) Validate() error {
	if c.MaxTokens <= 0 {
		return fmt.Errorf("session compress: MaxTokens must be >0")
	}
	if c.Ratio <= 0 || c.Ratio > 1 {
		return fmt.Errorf("session compress: Ratio must be (0,1]")
	}
	if c.KeepRecent < 0 {
		return fmt.Errorf("session compress: KeepRecent must be >=0")
	}
	if c.Compressor == nil {
		return fmt.Errorf("session compress: Compressor is nil")
	}
	return nil
}

// Threshold 返回触发阈值 token 数。
func (c Config) Threshold() int { return int(float64(c.MaxTokens) * c.Ratio) }

// EstimateTokens 估算消息列表的 token 数，近似 1 token ≈ 4 字符 + 每消息开销 4。
func EstimateTokens(msgs []agent.Message) int {
	total := 0
	for _, m := range msgs {
		for _, b := range m.Blocks {
			total += len(b.Text)
			total += len(b.Content)
			total += len(b.Input)
			total += len(b.Name)
			total += len(b.ToolUseID)
			total += len(b.ID)
			total += len(b.Signature)
		}
		total += 4 // 每消息开销
	}
	// 4 字符≈1 token，向上取整
	return (total + 3) / 4
}

// ShouldCompress 判断是否达到压缩阈值。
func ShouldCompress(msgs []agent.Message, cfg Config) bool {
	if err := cfg.Validate(); err != nil {
		return false
	}
	return EstimateTokens(msgs) >= cfg.Threshold()
}

// CompressMessages 执行压缩，返回新消息列表：[summary] + recent。
// summary 以 assistant 角色的 text 块承载，便于模型理解为历史摘要。
func CompressMessages(ctx context.Context, msgs []agent.Message, cfg Config) ([]agent.Message, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	keep := cfg.KeepRecent
	if keep <= 0 {
		keep = 6
	}
	if keep >= len(msgs) {
		return msgs, nil
	}
	split := len(msgs) - keep
	oldPart := msgs[:split]
	recentPart := msgs[split:]

	summary, err := cfg.Compressor.Compress(ctx, oldPart)
	if err != nil {
		return nil, fmt.Errorf("session compress: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("[compressed %d messages]", len(oldPart))
	}
	summaryMsg := agent.Message{
		Role: agent.RoleAssistant,
		Blocks: []agent.Block{
			{Type: agent.BlockText, Text: fmt.Sprintf("[session compressed: %d -> %d messages, summary]\n%s", len(oldPart), 1, summary)},
		},
	}
	out := make([]agent.Message, 0, 1+len(recentPart))
	out = append(out, summaryMsg)
	out = append(out, recentPart...)
	return out, nil
}

// NextCompressedName 生成压缩后的新会话名，规则：dev -> dev_1, dev_1 -> dev_2。
func NextCompressedName(base string, existing []string) string {
	taken := make(map[string]bool, len(existing))
	for _, n := range existing {
		taken[n] = true
	}
	// 已是 dev_数字 形式则递增
	stem, num := splitCounterUnderscore(base)
	// 若 base 本身不含下划线数字，stem=base, num=0
	for i := num + 1; ; i++ {
		cand := fmt.Sprintf("%s_%d", stem, i)
		if len(cand) > maxNameLen {
			cand = cand[:maxNameLen]
		}
		if !namePattern.MatchString(cand) {
			cand = fmt.Sprintf("%s_%d", stem[:maxNameLen-4], i)
		}
		if !taken[cand] {
			return cand
		}
	}
}

func splitCounterUnderscore(name string) (string, int) {
	// 匹配末尾 _数字
	idx := strings.LastIndex(name, "_")
	if idx > 0 && idx < len(name)-1 {
		suf := name[idx+1:]
		isNum := true
		for _, ch := range suf {
			if ch < '0' || ch > '9' {
				isNum = false
				break
			}
		}
		if isNum {
			var n int
			fmt.Sscanf(suf, "%d", &n)
			return name[:idx], n
		}
	}
	return name, 0
}

func messagePreview(m agent.Message) string {
	var parts []string
	for _, b := range m.Blocks {
		switch b.Type {
		case agent.BlockText:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case agent.BlockThinking:
			if b.Text != "" {
				parts = append(parts, "[thinking] "+b.Text)
			}
		case agent.BlockToolUse:
			parts = append(parts, fmt.Sprintf("[tool_use %s %s]", b.Name, string(b.Input)))
		case agent.BlockToolResult:
			parts = append(parts, fmt.Sprintf("[tool_result %s]", truncate(b.Content, 80)))
		}
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// MaybeCompress 检查是否需要压缩，若需要则生成新会话并保存，返回新会话名与新消息。
// 若无需压缩，返回 compressed=false。
func MaybeCompress(ctx context.Context, store *FileStore, currentName string, msgs []agent.Message, cfg Config) (newName string, newMsgs []agent.Message, compressed bool, err error) {
	if currentName == "" || len(msgs) == 0 {
		return "", nil, false, nil
	}
	if !ShouldCompress(msgs, cfg) {
		return "", nil, false, nil
	}
	newMsgs, err = CompressMessages(ctx, msgs, cfg)
	if err != nil {
		return "", nil, false, err
	}
	names, err := store.Names(ctx)
	if err != nil {
		return "", nil, false, err
	}
	newName = NextCompressedName(currentName, names)
	if err := store.Save(ctx, newName, newMsgs); err != nil {
		return "", nil, false, err
	}
	return newName, newMsgs, true, nil
}

// Ensure json import used for future extension
var _ = json.Marshal
