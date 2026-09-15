package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/holihur/agent/internal/agent"
)

// openaiError 是 OpenAI 错误体 {"error":{"message","type","code"}} 中的 error 部分。
// Chat Completions(老接口)与 Responses(新接口)共用同一错误形状。
type openaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// parseOpenAIError 解析 OpenAI 风格错误体;非该形状返回 nil。
func parseOpenAIError(raw []byte) *APIError {
	var env struct {
		Error *openaiError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Error == nil {
		return nil
	}
	return &APIError{Type: env.Error.Type, Message: env.Error.Message}
}

// openAIPost 以 Bearer 认证 POST 到 url(OpenAI 两接口统一走 Authorization: Bearer)。
func openAIPost(ctx context.Context, hc *http.Client, apiKey, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	if apiKey != "" {
		req.Header.Set("authorization", "Bearer "+apiKey)
	}
	c := hc
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: http: %w", err)
	}
	return resp, nil
}

// openAIErrorFromResponse 读取非 2xx 响应体并解析为 APIError(含 Status)。
func openAIErrorFromResponse(resp *http.Response) *APIError {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &APIError{Type: "http_error", Message: err.Error(), Status: resp.StatusCode}
	}
	if apiErr := parseOpenAIError(raw); apiErr != nil {
		apiErr.Status = resp.StatusCode
		return apiErr
	}
	return &APIError{Type: "http_error", Message: string(raw), Status: resp.StatusCode}
}

// chatArgumentsString 把 tool_use 入参归一为 OpenAI 的 JSON 字符串:
// 空/非法入参退化为 "{}"(OpenAI 要求 arguments 是 JSON 字符串)。
func chatArgumentsString(s string) string {
	if !json.Valid([]byte(s)) {
		return "{}"
	}
	return s
}

// Config 是所有协议适配器共享的构造参数。
// 装配层(CLI / 嵌入式门面)从 env/config/flag 归一后传入。
type Config struct {
	APIKey          string
	BaseURL         string
	Model           string
	MaxTokens       int
	AuthStyle       string       // 仅 anthropic 生效;openai/responses 恒为 Bearer
	Temperature     *float64     // nil = 不发送该字段(端点默认)
	ReasoningEffort string       // 空 = 不发送
	HTTP            *http.Client // 可注入(测试);nil 时用 http.DefaultClient
}

// NewAdapter 按 api 风格构造 wire 适配器,返回 agent.LLM port:
//   - "anthropic"(默认):POST {BaseURL}/v1/messages(既有 Messages API)
//   - "openai"(或 "chat"):POST {BaseURL}/v1/chat/completions(Chat Completions 老接口)
//   - "responses"(或 "openai-responses"):POST {BaseURL}/v1/responses(Responses API 新接口)
func NewAdapter(api string, cfg Config) (agent.LLM, error) {
	switch api {
	case "", "anthropic":
		c := New(cfg.APIKey, cfg.BaseURL, cfg.Model, cfg.MaxTokens)
		c.AuthStyle = cfg.AuthStyle
		c.Temperature = cfg.Temperature
		c.ReasoningEffort = cfg.ReasoningEffort
		c.HTTP = cfg.HTTP
		return c, nil
	case "openai", "chat":
		c := NewChat(cfg.APIKey, cfg.BaseURL, cfg.Model, cfg.MaxTokens)
		c.Temperature = cfg.Temperature
		c.ReasoningEffort = cfg.ReasoningEffort
		c.HTTP = cfg.HTTP
		return c, nil
	case "responses", "openai-responses":
		c := NewResponses(cfg.APIKey, cfg.BaseURL, cfg.Model, cfg.MaxTokens)
		c.Temperature = cfg.Temperature
		c.ReasoningEffort = cfg.ReasoningEffort
		c.HTTP = cfg.HTTP
		return c, nil
	default:
		return nil, fmt.Errorf("llm: unknown api %q (want anthropic|openai|responses)", api)
	}
}
