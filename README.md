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
