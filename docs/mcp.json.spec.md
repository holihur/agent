# 规范:mcp.json — MCP 服务器配置文件

状态:已实现(与 `internal/mcp/config.go`、`internal/mcp/http.go`、`cmd/agent/main.go` 同步演进,测试见 `internal/mcp/config_test.go`、`internal/mcp/http_test.go`、`cmd/agent/main_test.go`)。

## §mcp.json/目的

`mcp.json` 让 agent 在不写命令行参数的情况下声明要接入的 MCP stdio 服务器,格式与业界通用 `mcpServers` 配置(Claude Code、Cursor、VS Code 等)互通,现有配置文件可直接复用。

## §mcp.json/discovery

- 探测目录:进程当前工作目录(cwd),不向上遍历。
- 候选顺序:`mcp.json` → `.mcp.json`;**第一个存在的文件生效**,其余忽略。
- 两个候选都不存在:视作无文件配置,仅使用 `-mcp` flag(静默,非错误)。
- 找到的文件读取、解析或校验失败:fail-fast,进程以错误退出(见 §mcp.json/errors)。

## §mcp.json/format

顶层对象唯一必填键 `mcpServers`,其值是"服务器名 → 服务器定义"的对象。服务器定义二选一:`command`(stdio 子进程)或 `url`(Streamable HTTP 远程):

```json
{
  "mcpServers": {
    "echo": { "command": "/tmp/echo-mcp" },
    "fs": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      "env": { "FS_ROOT": "/tmp" }
    },
    "remote": {
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer tok" }
    }
  }
}
```

| 字段 | 类型 | 适用 | 必填 | 说明 |
|---|---|---|---|---|
| `command` | string | stdio | 二选一 | 可执行文件(由 agent 直接 exec) |
| `args` | string[] | stdio | 否 | 命令行参数 |
| `env` | object | stdio | 否 | 键值都必须是字符串;以键名排序转为 `K=V`,追加到子进程继承环境之后 |
| `url` | string | http | 二选一 | 必须 `http://` 或 `https://` 开头 |
| `headers` | object | http | 否 | 键值都必须是字符串;逐项附加到每个请求头(如 `Authorization`) |

- 顶层未知键与服务器级未知字段(如 `"type": "stdio"`、`"cwd"`)**容忍并忽略**,以兼容外部工具导出的配置。
- `env` 与 `headers` 错放到对方的传输类型 → 校验失败(见 §mcp.json/validation),防止配置写错传输类型。

## §mcp.json/validation

违反任一条即 fail-fast,错误信息必须包含文件路径与服务器名:

1. 服务器名匹配 `^[a-zA-Z0-9_-]+$`(与 tools 层命名空间一致)。
2. `command`(stdio)与 `url`(http)恰好一个,不得同时给出或都缺省。
3. `url` 必须 `http://` 或 `https://` 开头。
4. stdio 服务器的 `env`、http 服务器的 `headers` 键值都必须是字符串(JSON 数字/布尔/嵌套都拒绝)。
5. `env` 出现在 http 服务器、`headers` 出现在 stdio 服务器 → 报错。
6. 同一文件内服务器名不得重复。
7. `mcpServers` 必须存在且为对象(空对象合法,等于无文件配置)。

## §mcp.json/merge

`mcp.json` 条目与 `-mcp` flag 叠加:

- 注册顺序 = 文件条目按文档顺序在前,flag 条目按出现顺序追加在后。
- **同名时 flag 覆盖文件条目,且覆盖是条目级整体替换**(flag 条目没有 env/headers 概念,文件条目的 env/headers 不会保留;需要这些的服务器不要用同名 flag 覆盖)。
- flag 值以 `http(s)://` 开头 → 远程服务器,且不接受参数(`-mcp "x=https://h/mcp extra"` 报错)。
- 同名 flag 重复出现时后者覆盖前者(flag 解析层的既有行为)。

## §mcp.json/remote

Streamable HTTP 传输(`internal/mcp/http.go`)的行为契约:

- 每次请求 = 一个 `POST <url>`,头固定含 `Content-Type: application/json`、`Accept: application/json, text/event-stream`、`MCP-Protocol-Version: 2026-07-28`,外加配置的 `headers`。
- 会话:响应头 `Mcp-Session-Id` 一旦出现即被记住,后续请求原样回显。
- 响应三种形态都支持:`application/json` 单帧;`text/event-stream` 流(逐事件读 `data:` 载荷,丢弃通知与不匹配帧,直到本请求 id 的响应);通知 → `202 Accepted` 无帧。
- 与 stdio 共用时代探测(`server/discover` → modern;其他错误 → legacy 回退 `initialize`)与按需重生。
- **启动预检失败(不可达/协议不符)→ 向 stderr 警告并跳过该服务器,进程继续**;stdio 服务器失败仍是 fail-fast(命令不存在等属配置错误)。
- 单个 POST(含流读取)上限 60s,防远端挂死;`Close` 取消在途请求。
- 不订阅 GET 独立 SSE 流:服务器主动消息本客户端不消费(协议下服务器不发请求;通知无需实时送达)。

## §mcp.json/errors

| 场景 | 行为 |
|---|---|
| 两个候选文件都不存在 | 静默,无文件配置 |
| 第一个候选存在但解析失败 | fail-fast:`mcp: <path>: invalid JSON: ...` |
| 名称/command/url/校验失败 | fail-fast:`mcp: <path>: server "<name>": ...` |
| 远程服务器启动探测失败 | 警告 `mcp: skipping remote server "<name>": ...` 并跳过(不注册),进程继续 |
| stdio 服务器启动失败 | fail-fast:`tools: provider "<name>": ...` |
| flag 与文件同名 | flag 条目级替换文件条目,无警告 |

## §mcp.json/scope

- 支持 stdio 与 Streamable HTTP 两种传输;协议探测(现代 2026-07-28 → legacy 回退)由 `internal/mcp` 处理。
- 不做向上目录遍历;不订阅服务器主动消息流;不校验 command 是否可执行(exec 时才失败)、不校验 url 可达性(首请求时才失败)。
