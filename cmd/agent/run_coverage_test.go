package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestRun_EarlyErrors(t *testing.T) {
	// Test the early error paths for missing API key, base URL, model
	// We need to isolate the flag parsing and env
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no api key", map[string]string{}, []string{"agent"}, "no API key"},
		{"no base url", map[string]string{"LLM_API_KEY": "key"}, []string{"agent"}, "no base URL"},
		{"no model", map[string]string{"LLM_API_KEY": "key", "LLM_BASE_URL": "http://example.com"}, []string{"agent"}, "no model"},
		{"invalid shell", map[string]string{"LLM_API_KEY": "k", "LLM_BASE_URL": "http://a", "LLM_MODEL": "m"}, []string{"agent", "-shell", "invalid"}, "-shell must be"},
		{"invalid fs", map[string]string{"LLM_API_KEY": "k", "LLM_BASE_URL": "http://a", "LLM_MODEL": "m"}, []string{"agent", "-fs", "invalid"}, "-fs must be"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Save and restore
			oldArgs := os.Args
			oldFlag := flag.CommandLine
			// Save env
			savedEnv := map[string]string{}
			for k := range tc.env {
				savedEnv[k] = os.Getenv(k)
			}
			// Also save the ones we will clear
			for _, k := range []string{"LLM_API_KEY", "LLM_APIKEY", "LLM_BASE_URL", "LLM_MODEL", "LLM_PROVIDER"} {
				if _, ok := tc.env[k]; !ok {
					savedEnv[k] = os.Getenv(k)
					os.Unsetenv(k)
				}
			}
			for k, v := range tc.env {
				os.Setenv(k, v)
			}
			// Set args and reset flags
			os.Args = tc.args
			flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
			// Need to re-register flags as run() does
			// Instead of calling run() directly, we test the validation logic by calling a helper
			// For now, we just test that run() returns error containing want
			// Create a temp dir to avoid side effects
			tmpDir := t.TempDir()
			oldWd, _ := os.Getwd()
			os.Chdir(tmpDir)
			defer os.Chdir(oldWd)
			// Create a minimal .env to avoid interference
			os.WriteFile(filepath.Join(tmpDir, ".env"), []byte(""), 0644)
			err := run()
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("run() err = %v, want containing %q", err, tc.want)
			}
			// Restore
			os.Args = oldArgs
			flag.CommandLine = oldFlag
			for k, v := range savedEnv {
				if v == "" {
					os.Unsetenv(k)
				} else {
					os.Setenv(k, v)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
