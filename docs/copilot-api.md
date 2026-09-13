# Copilot Chat API (`copilot-api/1`)

gitdash 与 agent 运行时之间的**双向聊天协议**。双方只通过本协议交互，互不依赖
对方实现：agent 可以用其它语言重写，gitdash 也可以换成别的宿主。

- 传输：HTTP/1.1，上行 JSON、下行 `text/event-stream`（SSE）。
- 地址：由 agent 启动参数 `-api-addr` 指定（如 `127.0.0.1:8790`），gitdash 侧可配置。
- 工作区：agent 以启动时的工作目录为根（用 `-C <dir>` 指定为仓库克隆）。
- 版本：`GET /healthz` 返回的 `protocol` 字段。宿主应校验 `copilot-api/1`。

## agent 启动

```bash
LLM_API_KEY=... LLM_BASE_URL=https://api.anthropic.com LLM_MODEL=claude-sonnet-4-5 \
  agent -C /path/to/workspace -api-addr 127.0.0.1:8790
```

凭据也可经同名环境变量 `LLM_APIKEY`。该模式默认开启内置 `shell` 与
`read`/`write`/`edit` 文件工具（与本仓库其它模式不同）。

## 端点

### `GET /healthz`

```json
{ "ok": true, "protocol": "copilot-api/1", "sessions": 2 }
```

### `GET /api/sessions`

```json
{ "sessions": ["default", "42"] }
```

### `GET /api/messages?session=<name>`

回放该会话已持久化的对话历史。

```json
{
  "messages": [
    { "role": "user", "text": "add a README badge" },
    { "role": "assistant", "text": "Done.", "tools": [
      { "name": "edit", "input": "{\"edits\":[...]}", "result": "=== README.md: 1 edit(s) applied ===" }
    ]}
  ]
}
```

### `POST /api/chat`

请求：

```json
{ "session": "42", "text": "run the tests and fix failures" }
```

响应：`Content-Type: text/event-stream`，每帧 `data: <json>\n\n`，`json` 为：

| `type` | 字段 | 含义 |
| --- | --- | --- |
| `delta` | `text` | assistant 文本增量（可多次） |
| `tool_start` | `name`, `input` | 工具开始执行 |
| `tool_end` | `name`, `result`, `is_error` | 工具执行结束 |
| `done` | `text` | 本轮结束，附最终回答 |
| `error` | `error` | 本轮失败；`"canceled"` 表示被取消 |

同一 `session` 的并发 `/api/chat` 会被串行化（后到者等待）。

### `POST /api/cancel`

```json
{ "session": "42" }
```

取消该会话正在运行的一轮，返回 `{"canceled": true|false}`。

## 闭环约定

本协议只负责对话与工具执行。**仓库改动的提交/推送由宿主（gitdash）负责**：
宿主在会话工作区内完成一次 `/api/chat`（收到 `done`）后，自行 `git add/commit/push`。
agent 不感知 git。

## 独立 Web UI

`GET /` 提供一个无构建依赖的参考聊天页，直接消费 `/api/chat`；仅作演示/独立使用，
宿主可用自己的 UI 替换。
