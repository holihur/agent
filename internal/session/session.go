// Package session 实现会话持久化的文件适配器:实现 internal/agent 的
// SessionStore port,以目录下的 JSONL 文件存取完整对话历史
// (每个会话一个 <name>.jsonl,默认根 .agent/sessions)。
//
// 存储格式:一行一条消息,JSON 键名与 llm wire 层一致
// (role/blocks;type/text/id/name/input/tool_use_id/content/is_error/signature),
// thinking 签名原样往返,tool_result 块完整保留。写入走 tmp + rename 原子替换。
// 序列化在本包完成而非复用 llm:存储格式是本地持久化契约,不属于 Anthropic API wire 层。
package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/holihur/agent/internal/agent"
)

// namePattern 约束会话名([a-zA-Z0-9_-],1-64)防路径逃逸;与 tools 层命名空间校验同型。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// maxNameLen 与 namePattern 的 64 上限一致;NextName 用它封顶派生名长度。
const maxNameLen = 64

// FileStore 是 SessionStore 的文件实现:dir 内每个会话一个 <name>.jsonl。
type FileStore struct {
	dir string
}

// NewFileStore 返回以 dir 为根的文件会话存储;dir 在首次写入时创建。
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// Save 原子写入完整对话历史(整体替换旧内容;空历史写入空文件)。
func (s *FileStore) Save(_ context.Context, name string, msgs []agent.Message) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("session: invalid session name %q", name)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("session: mkdir %s: %w", s.dir, err)
	}
	var b strings.Builder
	for i, m := range msgs {
		line, err := json.Marshal(toStorage(m))
		if err != nil {
			return fmt.Errorf("session: encode %s msg %d: %w", name, i, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := s.path(name) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("session: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path(name)); err != nil {
		return fmt.Errorf("session: rename %s: %w", tmp, err)
	}
	return nil
}

// Load 读取完整对话历史;会话不存在时返回包装的 agent.ErrSessionNotFound。
func (s *FileStore) Load(_ context.Context, name string) ([]agent.Message, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("session: invalid session name %q", name)
	}
	f, err := os.Open(s.path(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("session %q: %w", name, agent.ErrSessionNotFound)
		}
		return nil, fmt.Errorf("session: open %s: %w", s.path(name), err)
	}
	defer f.Close()

	var msgs []agent.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 单行上限 16MB(长对话/大块)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var sm storageMessage
		if err := json.Unmarshal([]byte(text), &sm); err != nil {
			return nil, fmt.Errorf("session %s line %d: %w", name, line, err)
		}
		msgs = append(msgs, fromStorage(sm))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session: read %s: %w", s.path(name), err)
	}
	return msgs, nil
}

// Names 返回全部会话名(去 .jsonl 后缀,按名字排序);目录不存在返回空。
func (s *FileStore) Names(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read dir %s: %w", s.dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	slices.Sort(names)
	return names, nil
}

// Delete 删除会话文件;不存在时返回包装的 agent.ErrSessionNotFound。
func (s *FileStore) Delete(_ context.Context, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("session: invalid session name %q", name)
	}
	if err := os.Remove(s.path(name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("session %q: %w", name, agent.ErrSessionNotFound)
		}
		return fmt.Errorf("session: remove %s: %w", s.path(name), err)
	}
	return nil
}

func (s *FileStore) path(name string) string { return filepath.Join(s.dir, name+".jsonl") }

// NextName 返回 base 的下一个空闲会话名,供 REPL /new 轮转使用:
// base 尾部若带 "-<数字>" 计数则数字递增(work-3 → work-4),否则从
// work-2 起;与现有会话重名则继续递增,直到首个空闲名。
func NextName(ctx context.Context, s agent.SessionStore, base string) (string, error) {
	names, err := s.Names(ctx)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(names))
	for _, n := range names {
		taken[n] = true
	}
	stem, n := splitCounter(base)
	// 预留 "-" + 最多 10 位计数,派生名不越过 namePattern 的 64 上限
	if len(stem) > maxNameLen-12 {
		stem = stem[:maxNameLen-12]
	}
	for i := n + 1; ; i++ {
		cand := stem + "-" + strconv.Itoa(i)
		if !taken[cand] {
			return cand, nil
		}
	}
}

// splitCounter 拆出尾部 "-<数字>" 计数:"work-3" → ("work", 3);"work" → ("work", 1)。
func splitCounter(name string) (string, int) {
	if i := strings.LastIndex(name, "-"); i > 0 {
		if n, err := strconv.Atoi(name[i+1:]); err == nil {
			return name[:i], n
		}
	}
	return name, 1
}

// ---- 存储 wire(键名与 llm wire 层一致;域↔存储双向映射) ----

type storageBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

type storageMessage struct {
	Role   string         `json:"role"`
	Blocks []storageBlock `json:"blocks"`
}

func toStorage(m agent.Message) storageMessage {
	blocks := make([]storageBlock, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		blocks = append(blocks, storageBlock{
			Type: b.Type, Text: b.Text, ID: b.ID, Name: b.Name,
			Input: b.Input, ToolUseID: b.ToolUseID, Content: b.Content,
			IsError: b.IsError, Signature: b.Signature,
		})
	}
	return storageMessage{Role: string(m.Role), Blocks: blocks}
}

func fromStorage(sm storageMessage) agent.Message {
	blocks := make([]agent.Block, 0, len(sm.Blocks))
	for _, b := range sm.Blocks {
		blocks = append(blocks, agent.Block{
			Type: b.Type, Text: b.Text, ID: b.ID, Name: b.Name,
			Input: b.Input, ToolUseID: b.ToolUseID, Content: b.Content,
			IsError: b.IsError, Signature: b.Signature,
		})
	}
	return agent.Message{Role: agent.Role(sm.Role), Blocks: blocks}
}
