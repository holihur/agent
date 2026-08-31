package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 内置文件工具:read / write / edit,三者都支持批量(一次调用处理多个文件)。
//
// 与 shell 的分工:文件读写优先用这三个工具,shell 留给 git/grep/构建等命令。
// 批量语义各不同 ——
//   - read:  逐文件独立成败,单个失败不阻塞其他文件;
//   - write: 先全量轻校验(空 path、目标是目录),再逐个落盘,逐项报告;
//   - edit:  强原子 —— 全部 oldText 校验通过才落盘,任一失败则一个文件都不改。
//
// 原子性取舍的理由:read/write 是"写后即可重试失败项"的粗粒度操作,部分成功无害;
// edit 是精确手术,半应用状态会让模型基于错误的文件快照继续,全有或全无才能干净重试。

const (
	// maxFSOutput 是批量结果总输出上限,防止一次读爆 context(超出截断并提示)。
	maxFSOutput = 256 << 10
	// maxReadChunk 是单个文件单次读入的行数上限,防止大文件一次读入过多行;
	// 超限时结果提示模型用 offset 续读。
	maxReadChunk = 4000
	// maxLineLen 是单行读入上限(长行截断,如 minified 文件)。
	maxLineLen = 1 << 20
)

// RegisterFS 把内置文件工具(read/write/edit)注册到给定的进程内工具平面。
// 供 NewBuiltin 与嵌入式门面(agent.FS)共享同一实现。
func RegisterFS(p *LocalProvider) error {
	if err := p.Register(ToolDef{
		Name: "read",
		Description: "Read one or more files (batch: pass multiple paths). " +
			"Optional offset/limit apply to every file (1-based line numbers; default reads up to 4000 lines per file, " +
			"use offset to page through larger files). " +
			"Each file reports independently; a failed file does not block the others.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"paths": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
					"description": "Files to read, relative to the working directory",
				},
				"offset": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "First line to read (1-based; default 1)",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max lines to read per file (default: to end of file)",
				},
			},
			"required": []string{"paths"},
		},
	}, toolRead); err != nil {
		return err
	}
	if err := p.Register(ToolDef{
		Name: "write",
		Description: "Write one or more files (batch: pass multiple files), creating parent directories as needed. " +
			"Existing files are overwritten. Each file reports independently.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string"},
							"content": map[string]any{"type": "string"},
						},
						"required": []string{"path", "content"},
					},
				},
			},
			"required": []string{"files"},
		},
	}, toolWrite); err != nil {
		return err
	}
	return p.Register(ToolDef{
		Name: "edit",
		Description: "Apply exact text replacements across one or more files (batch: pass multiple edits). " +
			"Edits on the same file apply in array order, so later oldText may match text produced by an earlier edit. " +
			"Every oldText must match exactly once in its file; ambiguous or missing matches abort the whole call " +
			"and no file is modified (atomic). Include enough surrounding context to make oldText unique.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"edits": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string"},
							"oldText": map[string]any{"type": "string"},
							"newText": map[string]any{"type": "string"},
						},
						"required": []string{"path", "oldText", "newText"},
					},
				},
			},
			"required": []string{"edits"},
		},
	}, toolEdit)
}

// ---- read ----

func toolRead(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Paths  []string `json:"paths"`
		Offset int      `json:"offset"`
		Limit  int      `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.Paths) == 0 {
		return "", fmt.Errorf(`missing required argument "paths"`)
	}
	if args.Offset < 0 {
		return "", fmt.Errorf("offset must be >= 1, got %d", args.Offset)
	}
	offset := args.Offset
	if offset == 0 {
		offset = 1
	}

	var b strings.Builder
	ok, failed := 0, 0
	for _, path := range args.Paths {
		body, first, last, truncated, err := readChunk(path, offset, args.Limit)
		if err != nil {
			failed++
			fmt.Fprintf(&b, "=== %s: ERROR: %v ===\n", path, err)
			continue
		}
		ok++
		fmt.Fprintf(&b, "=== %s (lines %d-%d) ===\n", path, first, last)
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteByte('\n')
		}
		if truncated {
			fmt.Fprintf(&b, "... (stopped at line %d; file continues, use offset=%d to read the next chunk)\n", last, last+1)
		}
	}
	fmt.Fprintf(&b, "--- %d/%d files read ---\n", ok, ok+failed)
	return truncateOutput(b.String(), maxFSOutput), nil
}

// readChunk 读 path 从 offset 起的至多 limit 行(1-based;limit<=0 表示到文件尾)。
// 返回正文、实际首/尾行号;行数超过 maxReadChunk 时截断并置 truncated。
func readChunk(path string, offset, limit int) (body string, first, last int, truncated bool, err error) {
	// 未指定或超限时,默认/限制为 maxReadChunk 行(防大文件一次读爆)。
	if limit <= 0 || limit > maxReadChunk {
		limit = maxReadChunk
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxLineLen)

	// 跳过 offset-1 行。
	for skip := 1; skip < offset; skip++ {
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", 0, 0, false, err
			}
			// 文件行数不足 offset。
			return "", 0, 0, false, fmt.Errorf("file has fewer than %d lines", offset)
		}
	}

	var b strings.Builder
	first, last = offset, offset-1
	for limit <= 0 || last-first+1 < limit {
		if !sc.Scan() {
			break
		}
		if err := sc.Err(); err != nil {
			return "", 0, 0, false, err
		}
		last++
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	// 截断标志:读到上限行数停下,而非读到文件尾。
	truncated = last-first+1 >= limit
	return b.String(), first, last, truncated, nil
}

// ---- write ----

func toolWrite(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.Files) == 0 {
		return "", fmt.Errorf(`missing required argument "files"`)
	}

	// 全量轻校验:空 path 与"目标是已存在目录"提前报错,减少写到一半才发现问题。
	for i, f := range args.Files {
		if f.Path == "" {
			return "", fmt.Errorf("write: empty path in files[%d]", i)
		}
		if fi, err := os.Stat(f.Path); err == nil && fi.IsDir() {
			return "", fmt.Errorf("write: %q is a directory", f.Path)
		}
	}

	var b strings.Builder
	for _, f := range args.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return b.String(), fmt.Errorf("write: %s: create parent dirs: %w", f.Path, err)
		}
		if err := os.WriteFile(f.Path, []byte(f.Content), 0o644); err != nil {
			return b.String(), fmt.Errorf("write: %s: %w", f.Path, err)
		}
		fmt.Fprintf(&b, "=== %s: wrote %d bytes ===\n", f.Path, len(f.Content))
	}
	return b.String(), nil
}

// ---- edit ----

// editOp 是单条 edit,idx 保留数组原始顺序(同文件内按 idx 应用)。
type editOp struct {
	idx     int
	path    string
	oldText string
	newText string
}

// editGroup 是同一文件的一组 edit,按 idx 升序。
type editGroup struct {
	path  string
	edits []editOp
}

func toolEdit(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Edits []struct {
			Path    string `json:"path"`
			OldText string `json:"oldText"`
			NewText string `json:"newText"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.Edits) == 0 {
		return "", fmt.Errorf(`missing required argument "edits"`)
	}

	// 按 path 分组,组序取首次出现顺序,组内按 idx 升序。
	groups := groupEdits(args.Edits)

	// 阶段一:读入全部目标文件,组内顺序应用,全部成功才进入阶段二。
	contents := make(map[string]string, len(groups))
	for _, g := range groups {
		data, err := os.ReadFile(g.path)
		if err != nil {
			return "", fmt.Errorf("edit: %s: %w", g.path, err)
		}
		content := string(data)
		for _, e := range g.edits {
			if e.oldText == "" {
				return "", fmt.Errorf("edit #%d in %s: empty oldText", e.idx, g.path)
			}
			n := strings.Count(content, e.oldText)
			switch {
			case n == 0:
				return "", fmt.Errorf("edit #%d in %s: oldText not found: %q", e.idx, g.path, preview(e.oldText))
			case n > 1:
				return "", fmt.Errorf("edit #%d in %s: oldText is not unique (%d occurrences); add surrounding context: %q",
					e.idx, g.path, n, preview(e.oldText))
			}
			content = strings.Replace(content, e.oldText, e.newText, 1)
		}
		contents[g.path] = content
	}

	// 阶段二:全部校验通过,统一落盘(原子语义:阶段一任何失败都不会走到这里)。
	var b strings.Builder
	for _, g := range groups {
		if err := os.WriteFile(g.path, []byte(contents[g.path]), 0o644); err != nil {
			// 落盘失败(如磁盘满)仍可能半批;已写入的不回滚,逐项报告。
			fmt.Fprintf(&b, "=== %s: ERROR: %v ===\n", g.path, err)
			continue
		}
		fmt.Fprintf(&b, "=== %s: %d edit(s) applied ===\n", g.path, len(g.edits))
	}
	return b.String(), nil
}

// groupEdits 按 path 分组,组序取首次出现顺序,组内按数组顺序(idx)升序。
func groupEdits(edits []struct {
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}) []editGroup {
	order := []string{}
	byPath := map[string][]editOp{}
	for i, e := range edits {
		if _, seen := byPath[e.Path]; !seen {
			order = append(order, e.Path)
		}
		byPath[e.Path] = append(byPath[e.Path], editOp{idx: i, path: e.Path, oldText: e.OldText, newText: e.NewText})
	}
	groups := make([]editGroup, 0, len(order))
	for _, path := range order {
		ops := byPath[path]
		sort.Slice(ops, func(i, j int) bool { return ops[i].idx < ops[j].idx })
		groups = append(groups, editGroup{path: path, edits: ops})
	}
	return groups
}

// preview 截断 oldText 用于错误消息(避免整段刷屏)。
func preview(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
