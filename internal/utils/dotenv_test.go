package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := strings.Join([]string{
		"# comment",
		"",
		"DOTENV_A=plain",
		`DOTENV_B="quoted"`,
		`DOTENV_C='single'`,
		"  DOTENV_D =  spaced  ",
		"no_equals_line",
		"=no_key",
		"DOTENV_E=from-file",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOTENV_E", "from-env")

	LoadDotEnv(path)

	wants := map[string]string{
		"DOTENV_A": "plain",
		"DOTENV_B": "quoted",
		"DOTENV_C": "single",
		"DOTENV_D": "spaced",
		"DOTENV_E": "from-env",
	}
	for k, want := range wants {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"DOTENV_A", "DOTENV_B", "DOTENV_C", "DOTENV_D"} {
		_ = os.Unsetenv(k)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	LoadDotEnv(filepath.Join(t.TempDir(), "absent")) // 必须静默返回,不得 panic
}
