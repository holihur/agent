package hook

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

func TestInstallAllRunsInNameOrder(t *testing.T) {
	var ran []string
	Register("hook-b-test", func(*agent.Hooks, Deps) error { ran = append(ran, "b"); return nil })
	Register("hook-a-test", func(*agent.Hooks, Deps) error { ran = append(ran, "a"); return nil })
	t.Cleanup(func() {
		delete(registry, "hook-a-test")
		delete(registry, "hook-b-test")
	})

	if err := InstallAll(agent.NewHooks(), Deps{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ran, []string{"a", "b"}) {
		t.Fatalf("ran = %v, want [a b]", ran)
	}
}

func TestInstallAllWrapsError(t *testing.T) {
	Register("hook-err-test", func(*agent.Hooks, Deps) error { return errors.New("boom") })
	t.Cleanup(func() { delete(registry, "hook-err-test") })

	err := InstallAll(agent.NewHooks(), Deps{})
	if err == nil || !strings.Contains(err.Error(), "hook hook-err-test") {
		t.Fatalf("err = %v, want wrapped installer error", err)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on duplicate registration")
		}
	}()
	Register("hook-dup-test", func(*agent.Hooks, Deps) error { return nil })
	Register("hook-dup-test", func(*agent.Hooks, Deps) error { return nil })
}
