// agent init:交互式生成 cwd 下 .env(已存在则拒绝),引导首次上手。
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runInit 依次询问 LLM_BASE_URL / LLM_API_KEY / LLM_MODEL,写入 dir/.env。
func runInit(dir string, in io.Reader, out io.Writer) error {
	envPath := filepath.Join(dir, ".env")
	if _, err := os.Stat(envPath); err == nil {
		return fmt.Errorf("%s already exists: edit it directly or delete it before running `agent init`", envPath)
	}

	rd := bufio.NewReader(in)
	ask := func(label string) (string, error) {
		for {
			fmt.Fprintf(out, "%s: ", label)
			line, err := rd.ReadString('\n')
			if err != nil {
				return "", fmt.Errorf("read %s: %w", label, err)
			}
			v := strings.TrimSpace(line)
			if v != "" {
				return v, nil
			}
			fmt.Fprintf(out, "  (required, please enter a value)\n")
		}
	}
	baseURL, err := ask("LLM_BASE_URL (Anthropic-compatible endpoint, e.g. https://api.example.com)")
	if err != nil {
		return err
	}
	apiKey, err := ask("LLM_API_KEY")
	if err != nil {
		return err
	}
	model, err := ask("LLM_MODEL")
	if err != nil {
		return err
	}

	content := fmt.Sprintf("LLM_BASE_URL=%s\nLLM_API_KEY=%s\nLLM_MODEL=%s\n", baseURL, apiKey, model)
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s\nstart with: agent\n", envPath)
	return nil
}
