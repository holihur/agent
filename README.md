# agent

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

ag.OnTextDelta(func(d agent.TextDelta) { fmt.Print(d.Text) })
answer, err := ag.Run(context.Background(), "现在几点了?用 now 工具回答")
```

完整示例见 [examples/embedded](examples/README.md)。

## Skills

启动时扫描 cwd 下的 `.agents/skills/`:每个 `<name>/SKILL.md` 即一个技能
(YAML frontmatter 的 `name`/`description` 可选,`name` 缺省用目录名)。
技能清单注入 system prompt;模型可调用 `skill` 工具按需加载单个技能的完整指令。

```bash
agent -skills .agents/skills  # 默认;可改为其他目录(相对 cwd 或绝对路径)
agent -skills off             # 禁用
```

## Shell 开关

两个独立的 shell 入口,分别用各自的 flag 禁用(`off`/`none` 均可):

```bash
agent -shell off        # 禁用内置 shell 工具:模型不再能执行命令(MCP/skill 工具不受影响)
agent -shell-escape off # 禁用 REPL 的 "!" shell 逃逸:仅影响用户手动 !cmd,与 -shell 互不影响
```
