// Package memory 实现长期记忆的文件适配器:实现 internal/agent 的
// Memory port,以目录下的 JSON 文件按键存取文本记忆
// (每条记忆一个 <key>.json,默认根 .agent/memory)。
//
// 存储格式:单文件单条记忆 {key, value, updated}。写入走 tmp + rename
// 原子替换;Search 为大小写不敏感子串匹配(键与值均参与),语义检索留给
// 将来的向量实现 —— port 不变,新增一个适配器即可。
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/holihur/agent/internal/agent"
)

// keyPattern 约束记忆键([a-zA-Z0-9_-],1-64)防路径逃逸;与 session 层同型。
var keyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// FileStore 是 Memory 的文件实现:dir 内每条记忆一个 <key>.json。
type FileStore struct {
	dir string
}

// NewFileStore 返回以 dir 为根的文件记忆存储;dir 在首次写入时创建。
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// entry 是单条记忆的存储形状。
type entry struct {
	Key     string    `json:"key"`
	Value   string    `json:"value"`
	Updated time.Time `json:"updated"`
}

// Put 原子写入一条记忆(整体替换同名旧记忆)。
func (s *FileStore) Put(_ context.Context, key, value string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("memory: invalid memory key %q", key)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("memory: value must be non-empty")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", s.dir, err)
	}
	raw, err := json.Marshal(entry{Key: key, Value: value, Updated: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("memory: encode %s: %w", key, err)
	}
	tmp := s.path(key) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("memory: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path(key)); err != nil {
		return fmt.Errorf("memory: rename %s: %w", tmp, err)
	}
	return nil
}

// Get 读取一条记忆;key 不存在时返回包装的 agent.ErrMemoryNotFound。
func (s *FileStore) Get(_ context.Context, key string) (string, error) {
	if !keyPattern.MatchString(key) {
		return "", fmt.Errorf("memory: invalid memory key %q", key)
	}
	raw, err := os.ReadFile(s.path(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("memory %q: %w", key, agent.ErrMemoryNotFound)
		}
		return "", fmt.Errorf("memory: read %s: %w", s.path(key), err)
	}
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return "", fmt.Errorf("memory: decode %s: %w", s.path(key), err)
	}
	return e.Value, nil
}

// Search 按子串(大小写不敏感)在键与值中匹配,返回命中 key 列表(按 key 排序)。
func (s *FileStore) Search(ctx context.Context, query string) ([]string, error) {
	keys, err := s.Keys(ctx)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return keys, nil
	}
	var hits []string
	for _, k := range keys {
		if strings.Contains(strings.ToLower(k), q) {
			hits = append(hits, k)
			continue
		}
		v, err := s.Get(ctx, k)
		if err != nil {
			return nil, err
		}
		if strings.Contains(strings.ToLower(v), q) {
			hits = append(hits, k)
		}
	}
	return hits, nil
}

// Keys 返回全部记忆键(去 .json 后缀,按 key 排序);目录不存在返回空。
func (s *FileStore) Keys(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: read dir %s: %w", s.dir, err)
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		keys = append(keys, strings.TrimSuffix(e.Name(), ".json"))
	}
	slices.Sort(keys)
	return keys, nil
}

// Delete 删除一条记忆;不存在时返回包装的 agent.ErrMemoryNotFound。
func (s *FileStore) Delete(_ context.Context, key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("memory: invalid memory key %q", key)
	}
	if err := os.Remove(s.path(key)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("memory %q: %w", key, agent.ErrMemoryNotFound)
		}
		return fmt.Errorf("memory: remove %s: %w", s.path(key), err)
	}
	return nil
}

func (s *FileStore) path(key string) string { return filepath.Join(s.dir, key+".json") }
