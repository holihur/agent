package hook

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/holihur/agent/internal/agent"
)

var agentsMD = flag.String("agents-md", "auto", "AGENTS.md source: auto (discover from cwd up), off, or a file path (hook)")

func init() {
	Register("agents-md", installAgentsMD)
}

// installAgentsMD 启动时读取一次 AGENTS.md,之后每轮 Turn 都并入 system
// prompt(会话内保持一致,不随磁盘变化);实际加载到的路径打到 stderr。
func installAgentsMD(h *agent.Hooks, d Deps) error {
	content, loaded, err := loadAgentsMD(agentsMDSources(*agentsMD, d.CWD))
	if err != nil || content == "" {
		return err
	}
	h.OnMutateTurnRequest(func(r agent.TurnRequest) agent.TurnRequest {
		r.System = mergeAgentsMD(r.System, content)
		return r
	})
	if len(loaded) > 0 {
		fmt.Fprintf(os.Stderr, "agents-md: %s\n", strings.Join(loaded, ", "))
	}
	return nil
}

// agentsMDFile 是逐层查找的项目约定文件名(https://agents.md)。
const agentsMDFile = "AGENTS.md"

// agentsMDSources 按 -agents-md 模式解析指令来源,返回待读取的文件路径:
//
//	"off"/"none"  禁用,返回 nil;
//	""/"auto"     从 cwd 逐层向上发现(含 cwd 本身),外层在前、内层在后
//	              —— 越靠近 cwd 的文件越后出现,对模型优先级越高;
//	其余           视为显式文件路径,原样返回。
func agentsMDSources(mode, cwd string) []string {
	switch mode {
	case "off", "none":
		return nil
	case "", "auto":
		var paths []string
		for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
			if p := filepath.Join(dir, agentsMDFile); isRegularFile(p) {
				paths = append(paths, p)
			}
			if dir == filepath.Dir(dir) { // 已到文件系统根
				break
			}
		}
		slices.Reverse(paths)
		return paths
	default:
		return []string{mode}
	}
}

func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// loadAgentsMD 读取全部来源并拼装成一个文本:每个文件一节,节头标注来源路径
// (模型可据此判断"就近覆盖"),外层在前。空白文件跳过。
// 任一文件读取失败即报错(fail-fast)。
func loadAgentsMD(paths []string) (content string, loaded []string, err error) {
	var sections []string
	for _, p := range paths {
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return "", nil, fmt.Errorf("read %s: %w", p, readErr)
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			sections = append(sections, "# AGENTS.md ("+p+")\n\n"+text)
			loaded = append(loaded, p)
		}
	}
	return strings.Join(sections, "\n\n"), loaded, nil
}

// mergeAgentsMD 把 AGENTS.md 内容并入 system prompt:基础 prompt 在前,指令在后。
func mergeAgentsMD(base, extra string) string {
	switch {
	case strings.TrimSpace(extra) == "":
		return base
	case strings.TrimSpace(base) == "":
		return extra
	default:
		return strings.TrimRight(base, "\n") + "\n\n" + extra
	}
}
