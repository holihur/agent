// Package mcp 是手写的 MCP(2026-07-28,无状态版)客户端适配层,
// 实现 internal/tools 的 Provider 接口。本包是唯一出现 MCP wire 细节的地方。
//
// 规范锚点:https://modelcontextprotocol.io/specification/2026-07-28
//   - 无状态:无 initialize 握手;每个请求 params._meta 自带协议版本等必填字段
//   - stdio:子进程 stdin/stdout 上的换行分隔 JSON-RPC(NDJSON),stderr 仅为日志
//   - 服务器不主动发 JSON-RPC 请求;MRTR 用 InputRequiredResult 承载
//   - 时代探测:先探 server/discover,非现代错误 ⇒ 遗留服务器(本客户端不支持)
package mcp

import "context"

// Transport 抽象一帧传输(MCP 消息 = 一行 JSON)。协议与载体解耦:
// stdio 是子进程管道(internal/mcp/stdio.go);
// Streamable HTTP 是 POST + 单帧/SSE 响应(internal/mcp/http.go)。
type Transport interface {
	// Send 写一帧(实现方保证并发安全)。
	Send(b []byte) error
	// Recv 读一帧,阻塞;io.EOF/errClosed 表示通道关闭。
	Recv() ([]byte, error)
	// Close 关闭通道(stdio:按规范"关 stdin → 等待 → 超时强杀")。
	Close() error
}

// DialFunc 是传输工厂:Provider 在按需(重)连时调用。
type DialFunc func(ctx context.Context) (Transport, error)
