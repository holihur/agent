package pprof

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
)

func TestResolveAddr(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"off":           "",
		"none":          "",
		"on":            "localhost:6060",
		"127.0.0.1:0":   "127.0.0.1:0",
		"localhost:999": "localhost:999",
	}
	for mode, want := range cases {
		if got := resolveAddr(mode); got != want {
			t.Errorf("resolveAddr(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestInstallDisabled(t *testing.T) {
	for _, mode := range []string{"", "off", "none"} {
		old := *pprofAddr
		defer func() { *pprofAddr = old }()
		*pprofAddr = mode

		servingAddr = ""
		if err := installPprof(agent.NewHooks(), hook.Deps{}); err != nil {
			t.Fatalf("installPprof(%q): %v", mode, err)
		}
		if servingAddr != "" {
			t.Fatalf("mode %q must not serve, got %q", mode, servingAddr)
		}
	}
}

func TestInstallServesPprof(t *testing.T) {
	old := *pprofAddr
	defer func() { *pprofAddr = old; servingAddr = "" }()
	*pprofAddr = "127.0.0.1:0"

	servingAddr = ""
	if err := installPprof(agent.NewHooks(), hook.Deps{}); err != nil {
		t.Fatalf("installPprof: %v", err)
	}
	if servingAddr == "" {
		t.Fatal("enabled install must record the bound address")
	}

	resp, err := http.Get("http://" + servingAddr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Types of profiles available") {
		t.Fatalf("pprof index body unexpected: %.200s", body)
	}
}
