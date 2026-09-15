package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppConfig_MissingFile(t *testing.T) {
	cfg, err := loadAppConfig(filepath.Join(t.TempDir(), "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != nil || cfg.MaxTokens != nil || cfg.Temperature != nil {
		t.Fatal("expected zero-value config for missing file")
	}
}

func TestLoadAppConfig_ParsesFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	content := `{"model":"m1","max_tokens":4096,"temperature":0.3,"session":"work","compress_ratio":0.9,"api":"openai"}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadAppConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model == nil || *cfg.Model != "m1" {
		t.Fatalf("model = %v", cfg.Model)
	}
	if cfg.MaxTokens == nil || *cfg.MaxTokens != 4096 {
		t.Fatalf("max_tokens = %v", cfg.MaxTokens)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.3 {
		t.Fatalf("temperature = %v", cfg.Temperature)
	}
	if cfg.Session == nil || *cfg.Session != "work" {
		t.Fatalf("session = %v", cfg.Session)
	}
	if cfg.CompressRatio == nil || *cfg.CompressRatio != 0.9 {
		t.Fatalf("compress_ratio = %v", cfg.CompressRatio)
	}
	if cfg.Provider != nil {
		t.Fatalf("unset provider should be nil, got %v", *cfg.Provider)
	}
	if cfg.API == nil || *cfg.API != "openai" {
		t.Fatalf("api = %v", cfg.API)
	}
}

func TestLoadAppConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAppConfig(path); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestRunInit_WritesEnv(t *testing.T) {
	dir := t.TempDir()
	in := strings.NewReader("https://api.example.com\nsk-test\nmy-model\n")
	out := &strings.Builder{}
	if err := runInit(dir, in, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	want := "LLM_BASE_URL=https://api.example.com\nLLM_API_KEY=sk-test\nLLM_MODEL=my-model\n"
	if string(data) != want {
		t.Fatalf(".env = %q, want %q", data, want)
	}
}

func TestRunInit_RefusesExistingEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInit(dir, strings.NewReader(""), &strings.Builder{}); err == nil {
		t.Fatal("expected error when .env exists")
	}
}

func TestRunInit_RetriesEmptyInput(t *testing.T) {
	dir := t.TempDir()
	in := strings.NewReader("\n\nhttps://api.example.com\nsk-test\nmy-model\n")
	out := &strings.Builder{}
	if err := runInit(dir, in, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(required") {
		t.Fatal("expected empty-input retry hint in output")
	}
}
