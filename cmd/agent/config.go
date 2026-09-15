// appConfig 是 agent.json 配置文件:cwd 下 agent.json(可选,缺省跳过)。
// 配置值作为 flag 默认值生效,命令行 flag 始终可覆盖;JSON 里未写或为 null 的字段不生效。
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type appConfig struct {
	Provider        *string  `json:"provider"`
	API             *string  `json:"api"`
	Model           *string  `json:"model"`
	MaxTokens       *int     `json:"max_tokens"`
	MaxTurns        *int     `json:"max_turns"`
	Temperature     *float64 `json:"temperature"`
	ReasoningEffort *string  `json:"reasoning_effort"`
	Session         *string  `json:"session"`
	SessionCompress *string  `json:"session_compress"`
	CompressMaxToks *int     `json:"compress_max_tokens"`
	CompressRatio   *float64 `json:"compress_ratio"`
	CompressKeep    *int     `json:"compress_keep"`
}

// loadAppConfig 读取 cwd 下 agent.json;文件不存在返回零值配置(nil 指针 = 未配置)。
func loadAppConfig(path string) (*appConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &appConfig{}, nil
		}
		return nil, err
	}
	var cfg appConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// stringOr 返回配置值或回退默认。
func stringOr(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func floatOr(p *float64, def float64) float64 {
	if p != nil {
		return *p
	}
	return def
}
