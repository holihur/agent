# 示例:echo-mcp

最小 MCP stdio 服务器(现代 2026-07-28 协议,零第三方依赖),演示 NDJSON 分帧、`server/discover` 探测、`tools/list` 与 `tools/call`。提供两个工具:

| 工具 | 参数 | 行为 |
|---|---|---|
| `echo` | `{"text": string}` | 原样返回文本 |
| `now` | 无 | 返回服务器当前时间(RFC3339) |

## 方式一:`-mcp` flag

```bash
go build -o /tmp/echo-mcp ./examples/echo
go run ./cmd/agent -mcp "echo=/tmp/echo-mcp" -q "用 echo 工具返回 ping"
```

REPL 同理:

```bash
go run ./cmd/agent -mcp "echo=/tmp/echo-mcp"
> 用 now 工具告诉我现在几点
```

## 方式二:`mcp.json`(规范见 `docs/mcp.json.spec.md`)

在运行目录放一个 `mcp.json`:

```json
{
  "mcpServers": {
    "echo": { "command": "/tmp/echo-mcp" }
  }
}
```

然后直接启动,agent 自动加载并连接:

```bash
go run ./cmd/agent -q "用 echo 工具把 '你好' 原样返回"
```

启动后 stderr 会打印预检的工具列表(内置工具 + `echo`/`now`),说明 MCP 服务器已接入。

## 方式三:远程(Streamable HTTP)`examples/echo-http`

echo-http 与 stdio 版工具相同,但走 HTTP:POST 进、SSE 事件出。

```bash
go run ./examples/echo-http &            # 监听 http://127.0.0.1:8787/mcp
go run ./cmd/agent -mcp "echo=http://127.0.0.1:8787/mcp" -q "用 now 工具告诉我现在几点"
```

或经 mcp.json(远程服务器用 `url` + 可选 `headers`):

```json
{
  "mcpServers": {
    "echo": { "url": "http://127.0.0.1:8787/mcp" }
  }
}
```

## 方式四:嵌入式(Library)`examples/embedded`

不经 CLI,宿主 Go 程序在进程内装配并驱动 agent(配置全可选,凭据走 env):

```bash
LLM_API_KEY=... LLM_BASE_URL=... LLM_MODEL=... go run ./examples/embedded
```

公开 API 见模块根的 `agent` 包文档(`New` / `Tool` / `MCP` / `Shell` / `FS` / `OnTextDelta` / `Run` / `Close`)。

## 说明

- 两个示例都只实现 agent 客户端走过的现代路径(`server/discover` → `tools/list` → `tools/call`),不是通用 MCP 服务器,legacy(2025-06-18)握手未实现。
- 也可以不经编译直接用 `go run`:`-mcp "echo=go run ./examples/echo"`(启动稍慢)。
- 传输层测试覆盖:`internal/mcp/http_test.go`(SSE/JSON 往返、session 回显、鉴权头、错误映射、通知 202、Close 中断)。
