package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

// TestRun_APIStyleSelection 覆盖 -api flag 与 LLM_API env 的协议选择分支:
// 合法取值走到 LLM 调用(fake URL 报错即可),非法取值在 NewAdapter 处报错。
func TestRun_APIStyleSelection(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string // 空 = 不校验错误文本(仅确保走到 LLM wiring)
	}{
		{"flag anthropic", nil, []string{"agent", "-api", "anthropic", "-q", "hi", "-shell", "off", "-fs", "off"}, ""},
		{"flag openai", nil, []string{"agent", "-api", "openai", "-q", "hi", "-shell", "off", "-fs", "off"}, ""},
		{"flag responses", nil, []string{"agent", "-api", "responses", "-q", "hi", "-shell", "off", "-fs", "off"}, ""},
		{"env fallback", map[string]string{"LLM_API": "responses"}, []string{"agent", "-q", "hi", "-shell", "off", "-fs", "off"}, ""},
		{"invalid", nil, []string{"agent", "-api", "bogus", "-q", "hi", "-shell", "off", "-fs", "off"}, "unknown api"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			oldWd, _ := os.Getwd()
			os.Chdir(tmpDir)
			defer os.Chdir(oldWd)

			oldArgs := os.Args
			oldFlag := flag.CommandLine
			defer func() {
				os.Args = oldArgs
				flag.CommandLine = oldFlag
			}()

			// 清空相关 env(t.Setenv 自动还原),避免宿主环境串扰。
			for _, k := range []string{"LLM_API_KEY", "LLM_APIKEY", "LLM_BASE_URL", "LLM_MODEL", "LLM_API", "LLM_PROVIDER"} {
				t.Setenv(k, "")
			}
			t.Setenv("LLM_API_KEY", "k")
			t.Setenv("LLM_BASE_URL", "http://127.0.0.1:1")
			t.Setenv("LLM_MODEL", "m")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			os.Args = tc.args
			flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)

			err := run()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("run() err = %v, want containing %q", err, tc.wantErr)
				}
			}
			// 合法 api 时走到 LLM 调用即覆盖 wiring;错误文本不强制。
		})
	}
}
