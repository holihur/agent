package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/holihur/agent/internal/agent"
)

const (
	anthropicVersion = "2023-06-01" // anthropic-version 头的当前标准值
	defaultMaxTokens = 1024
)

// Client 实现 agent.LLM,封装一次 POST {BaseURL}/v1/messages。
// model / max_tokens / api key 在构造时注入,不随每轮传递(port 设计)。
type Client struct {
	APIKey    string
	BaseURL   string
	Model     string
	MaxTokens int
	HTTP      *http.Client // 可注入(测试);nil 时用 http.DefaultClient
	// AuthStyle 决定认证头:bearer(默认)| x-api-key | both。
	// 各网关认证方式不同,无法从 URL 可靠推断,故显式配置。
	AuthStyle string
}

func New(apiKey, baseURL, model string, maxTokens int) *Client {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &Client{APIKey: apiKey, BaseURL: baseURL, Model: model, MaxTokens: maxTokens}
}

// Turn 发起一次 /v1/messages 调用,返回 assistant 消息与 stop_reason。
func (c *Client) Turn(ctx context.Context, r agent.TurnRequest) (agent.TurnResult, error) {
	if c.BaseURL == "" {
		return agent.TurnResult{}, errors.New("llm: base URL is required")
	}
	body, err := json.Marshal(wireRequest{
		Model:     c.Model,
		MaxTokens: c.MaxTokens,
		System:    r.System,
		Messages:  domainToWireMessages(r.Messages),
		Tools:     specsToWireTools(r.Tools),
	})
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: encode request: %w", err)
	}

	resp, err := c.post(ctx, body)
	if err != nil {
		return agent.TurnResult{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if apiErr := parseErrorBody(raw); apiErr != nil {
			apiErr.Status = resp.StatusCode
			return agent.TurnResult{}, apiErr
		}
		return agent.TurnResult{}, &APIError{Type: "http_error", Message: string(raw), Status: resp.StatusCode}
	}

	var probe struct {
		Type       string        `json:"type"`
		Error      *wireAPIError `json:"error"`
		Role       string        `json:"role"`
		Content    []wireBlock   `json:"content"`
		StopReason string        `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return agent.TurnResult{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if probe.Type == "error" && probe.Error != nil {
		// 2xx 但错误体:按协议错误处理
		return agent.TurnResult{}, &APIError{Type: probe.Error.Type, Message: probe.Error.Message, Status: resp.StatusCode}
	}

	blocks, err := wireToDomainBlocks(probe.Content)
	if err != nil {
		return agent.TurnResult{}, err
	}
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: blocks},
		StopReason: probe.StopReason,
	}, nil
}

func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	switch c.AuthStyle {
	case "x-api-key":
		req.Header.Set("x-api-key", c.APIKey)
	case "both":
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("authorization", "Bearer "+c.APIKey)
	default:
		req.Header.Set("authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: http: %w", err)
	}
	return resp, nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// parseErrorBody 解析 {"type":"error","error":{"type","message"}};非该形状返回 nil。
func parseErrorBody(raw []byte) *APIError {
	var env struct {
		Type  string        `json:"type"`
		Error *wireAPIError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Type != "error" || env.Error == nil {
		return nil
	}
	return &APIError{Type: env.Error.Type, Message: env.Error.Message}
}
