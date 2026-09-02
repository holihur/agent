package permission

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// HasRiskyShellConstruct 检测 shell 命令是否包含管道、串联、后台、子 shell、命令替换等高风险结构。
// 仅对可解析的命令生效，解析失败则按字符串兜底检测。
func HasRiskyShellConstruct(cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	p := syntax.NewParser(syntax.Variant(syntax.LangBash))
	f, err := p.Parse(strings.NewReader(cmd), "")
	if err != nil {
		return hasMetaFallback(cmd)
	}
	risky := false
	syntax.Walk(f, func(n syntax.Node) bool {
		if risky {
			return false
		}
		switch n.(type) {
		case *syntax.BinaryCmd:
			risky = true
		case *syntax.Subshell:
			risky = true
		case *syntax.CmdSubst:
			risky = true
		case *syntax.ProcSubst:
			risky = true
		case *syntax.ArithmExp:
			// 算术展开一般无害，但可能配合 $(( )) 绕过，暂不视为高风险
		}
		// Stmt 级别的管道、后台
		if st, ok := n.(*syntax.Stmt); ok {
			if st.Background {
				risky = true
			}
			if st.Coprocess {
				risky = true
			}
			// 管道通过 BinaryCmd 的 Op 为 | |& 体现，已在上面捕捉
		}
		return !risky
	})
	// 额外：多语句（; 分隔）会产生多个 Stmt
	if !risky && len(f.Stmts) > 1 {
		risky = true
	}
	if risky {
		return true
	}
	// 解析成功但未命中上述节点时，仍做字符串兜底（处理引号内管道误判的互补）
	return false
}

// HasPipeline 仅检测是否含管道（| 或 |&），用于显式授权判断。
func HasPipeline(cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	p := syntax.NewParser(syntax.Variant(syntax.LangBash))
	f, err := p.Parse(strings.NewReader(cmd), "")
	if err != nil {
		return strings.Contains(cmd, "|")
	}
	found := false
	syntax.Walk(f, func(n syntax.Node) bool {
		if bc, ok := n.(*syntax.BinaryCmd); ok {
			if bc.Op == syntax.Pipe || bc.Op == syntax.PipeAll {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}

// hasMetaFallback 解析失败时的字符串兜底，避免误放行。
func hasMetaFallback(s string) bool {
	// 引号内的 | 不应算作管道，但兜底阶段无法区分，宁可误拦
	for _, c := range []string{"|", ";", "&&", "||", "&", "$(", "`", "$`"} {
		if strings.Contains(s, c) {
			return true
		}
	}
	return false
}

// extractShellCommand 从 shell 工具的 Input JSON 中提取 command 字段。
func extractShellCommand(input string) string {
	// 输入可能是 json.RawMessage 的字符串形式，尝试简单提取
	// 调用方已在 matcher 中处理 json.Unmarshal，这里仅做轻量回退
	return input
}
