# agent

[![CI](https://github.com/holihur/agent/actions/workflows/ci.yml/badge.svg)](https://github.com/holihur/agent/actions/workflows/ci.yml)
[![Coverage](https://codecov.io/gh/holihur/agent/graph/badge.svg)](https://codecov.io/gh/holihur/agent)

## 安装

```bash
go install github.com/holihur/agent/cmd/agent@latest
```

## MCP 接入

```bash
# 命令行 flag(可重复;stdio 子进程或远程 http(s) URL)
agent -mcp "fs=npx @modelcontextprotocol/server-filesystem /tmp"
agent -mcp "remote=https://mcp.example.com/mcp"

# 或 cwd 下 mcp.json(规范见 docs/mcp.json.spec.md)
```

完整示例(stdio 与 Streamable HTTP 两种传输)见 [examples](examples/README.md)。

## 会话持久化

对话历史按会话名存为 cwd 下 `.agent/sessions/<name>.jsonl`(JSONL,一行一条消息):

```bash
agent -sessions        # 列出已保存会话
agent -session work    # 续接会话 work(不存在则新建),每轮自动保存;REPL 与 -q 均可
```

## 嵌入式(Library)

外部 Go 程序可在进程内直接驱动 agent(零 CLI、零 flag,配置全可选):

```go
import agent "github.com/holihur/agent"

ag, err := agent.New() // 凭据缺省走 env:LLM_API_KEY / LLM_BASE_URL / LLM_MODEL
if err != nil { log.Fatal(err) }
defer ag.Close()

_ = ag.Tool("now", "Returns the current time in RFC3339.",
    map[string]any{"type": "object"},
    func(_ context.Context, _ json.RawMessage) (string, error) {
        return time.Now().Format(time.RFC3339), nil
    })
_ = ag.MCP(agent.MCPSpec{Name: "echo", Command: []string{"/tmp/echo-mcp"}})
_ = ag.Shell() // 内置 shell 工具默认关闭,按需开启
_ = ag.FS()    // 内置文件工具 read/write/edit,默认关闭,按需开启(均支持批量)

ag.OnTextDelta(func(d agent.TextDelta) { fmt.Print(d.Text) })
answer, err := ag.Run(context.Background(), "现在几点了?用 now 工具回答")
```

完整示例见 [examples/embedded](examples/README.md)。

## 终端 Markdown 渲染(Streamdown Go 版)

[pkg/streamdown](pkg/streamdown) 是 [day50-dev/render-markdown-terminal](https://github.com/day50-dev/render-markdown-terminal)
(Streamdown)的 Go 移植:把 markdown 流式渲染成带 ANSI 颜色的终端文本,
支持流式增量输出(LLM token 边到边渲染)。

```go
import "github.com/holihur/agent/pkg/streamdown"

r, _ := streamdown.New(os.Stdout) // 默认调色板与排版,0 值 Config 即全默认
r.RenderString(markdown)          // 或 r.Render(io.Reader) 流式读取
r.Tidyup()                        // 收尾:复位终端,可选 OSC 52 剪贴板
```

特性与上游一致:HSV 推导真彩调色板、CJK 感知换行(宽度表与 Python wcwidth 逐字节一致)、
表格/列表(含嵌套)/引用/标题(ATX+setext)/分隔线/代码块(``` 与 4 空格缩进,
chroma 语法高亮,▄/▀ 半块边衬)、行内样式(粗体/斜体/删除线/下划线/行内代码/上标脚注)、
OSC 8 超链接。差异:`Config` 零值安全(`DefaultConfig()` 见包文档);语法高亮用
chroma 而非 pygments(颜色有出入,结构一致);默认色相 0.6(蓝族)——上游默认 0.8
(品红族)在终端上代码块/表格背景偏暗枣红,想还原原配色传 `HSV = [0.8 0.5 0.5]`;
镜像内嵌协议不支持,图片回退为 alt 文本。

## Skills

启动时扫描 cwd 下的 `.agents/skills/`:每个 `<name>/SKILL.md` 即一个技能
(YAML frontmatter 的 `name`/`description` 可选,`name` 缺省用目录名)。
技能清单注入 system prompt;模型可调用 `skill` 工具按需加载单个技能的完整指令。

```bash
agent -skills .agents/skills  # 默认;可改为其他目录(相对 cwd 或绝对路径)
agent -skills off             # 禁用
```

## 内置文件工具

三个内置工具 read / write / edit,均支持批量(一次调用处理多个文件):

- `read`: 传 `paths` 数组批量读,可选 `offset`/`limit`(1-based 行号)分段读大文件;每文件独立成败。
- `write`: 传 `files: [{path, content}]` 批量写,自动创建父目录,覆盖已有文件。
- `edit`: 传 `edits: [{path, oldText, newText}]` 批量做精确替换;同文件按序应用(支持链式),任一 `oldText` 缺失或不唯一则整批不落盘(原子)。

文件读写优先用这三个工具;shell 留给 git/grep/构建/列目录等命令。

## Shell 开关

两个独立的 shell 入口,分别用各自的 flag 禁用(`off`/`none` 均可):

```bash
agent -shell off        # 禁用内置 shell 工具:模型不再能执行命令(MCP/skill 工具不受影响)
agent -shell-escape off # 禁用 REPL 的 "!" shell 逃逸:仅影响用户手动 !cmd,与 -shell 互不影响
agent -fs off           # 禁用内置文件工具 read/write/edit:模型不再能直接读写文件
```

## Markdown 渲染

回答(流式与一次性)默认经 `pkg/streamdown` 渲染成带 ANSI 颜色的终端 markdown
(标题/表格/列表/引用/代码块等,见上节)。`auto` 下仅在输出为 TTY 时开启:

```bash
agent -markdown on     # 强制开启(管道输出也渲染)
agent -markdown off    # 关闭,原文直出
agent -markdown auto   # 默认:stdout 为 TTY 时开启
```

## REPL 命令

交互模式下,以 `/` 开头的输入按命令处理(拦截在对话循环之前,不进历史、不调模型):

```bash
/help        # 打印帮助文档(列出全部 REPL 命令)
/exit /quit  # 退出(裸 exit/quit 亦可)
!cmd         # shell 逃逸,如 !git status(见上节)
```

未知命令给出 `unknown command` 提示并指向 `/help`。`agent -slashcmd off` 可整体禁用 `/` 命令。
