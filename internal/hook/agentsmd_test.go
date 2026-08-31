package hook

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent/internal/agent"
	"agent/internal/tools"
)

func writeAgentsMD(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, agentsMDFile)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// filterUnder 只保留 root 树内的路径:发现逻辑会走到文件系统根,
// 测试机上任何祖先目录的杂散 AGENTS.md 都不应影响对本单元行为的断言。
func filterUnder(paths []string, root string) []string {
	var out []string
	for _, p := range paths {
		if strings.HasPrefix(p, root+string(os.PathSeparator)) {
			out = append(out, p)
		}
	}
	return out
}

func TestAgentsMDSourcesAuto(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentsMD(t, root, "outer")
	writeAgentsMD(t, deep, "inner") // a 层无文件,应被跳过

	got := filterUnder(agentsMDSources("auto", deep), root)
	want := []string{filepath.Join(root, agentsMDFile), filepath.Join(deep, agentsMDFile)}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAgentsMDSourcesModes(t *testing.T) {
	if got := agentsMDSources("off", "/x"); got != nil {
		t.Fatalf("off: got %v, want nil", got)
	}
	if got := agentsMDSources("none", "/x"); got != nil {
		t.Fatalf("none: got %v, want nil", got)
	}
	if got := agentsMDSources("/some/AGENTS.md", "/x"); !slices.Equal(got, []string{"/some/AGENTS.md"}) {
		t.Fatalf("explicit: got %v", got)
	}
}

func TestLoadAgentsMD(t *testing.T) {
	dir := t.TempDir()
	outer := writeAgentsMD(t, dir, "outer rules")

	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := writeAgentsMD(t, sub, "inner rules")

	blank := filepath.Join(dir, "blank")
	if err := os.MkdirAll(blank, 0o755); err != nil {
		t.Fatal(err)
	}
	blankPath := writeAgentsMD(t, blank, " \n\t ")

	content, loaded, err := loadAgentsMD([]string{outer, blankPath, inner})
	if err != nil {
		t.Fatal(err)
	}
	want := "# AGENTS.md (" + outer + ")\n\nouter rules\n\n" +
		"# AGENTS.md (" + inner + ")\n\ninner rules"
	if content != want {
		t.Fatalf("content =\n%s\nwant\n%s", content, want)
	}
	if !slices.Equal(loaded, []string{outer, inner}) {
		t.Fatalf("loaded = %v, want %v", loaded, []string{outer, inner})
	}
}

func TestLoadAgentsMDFailFast(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", agentsMDFile)
	if _, _, err := loadAgentsMD([]string{missing}); err == nil {
		t.Fatal("want error for unreadable source")
	}
}

func TestMergeAgentsMD(t *testing.T) {
	cases := []struct{ base, extra, want string }{
		{"base", "rules", "base\n\nrules"},
		{"base\n", "rules", "base\n\nrules"},
		{"", "rules", "rules"},
		{" \n", "rules", "rules"},
		{"base", "", "base"},
		{"base", "  ", "base"},
	}
	for _, c := range cases {
		if got := mergeAgentsMD(c.base, c.extra); got != c.want {
			t.Fatalf("merge(%q, %q) = %q, want %q", c.base, c.extra, got, c.want)
		}
	}
}

// captureLLM 记录收到的请求后立即结束回合,使 Agent.Run 无需真实 LLM。
type captureLLM struct{ got agent.TurnRequest }

func (c *captureLLM) Turn(_ context.Context, r agent.TurnRequest) (agent.TurnResult, error) {
	c.got = r
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText("ok")}},
		StopReason: "end_turn",
	}, nil
}

// setAgentsMDFlag 临时改写 -agents-md flag 全局,测试结束恢复。
func setAgentsMDFlag(t *testing.T, v string) {
	t.Helper()
	old := *agentsMD
	*agentsMD = v
	t.Cleanup(func() { *agentsMD = old })
}

func TestInstallAgentsMDInjectsSystem(t *testing.T) {
	p := writeAgentsMD(t, t.TempDir(), "always answer in haiku")
	setAgentsMDFlag(t, p)

	h := agent.NewHooks()
	if err := installAgentsMD(h, Deps{CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	cap := &captureLLM{}
	ag := &agent.Agent{LLM: cap, Registry: tools.New(), System: "base prompt", Hooks: h}
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cap.got.System, "base prompt") {
		t.Fatalf("base prompt lost: %q", cap.got.System)
	}
	if !strings.Contains(cap.got.System, "always answer in haiku") {
		t.Fatalf("AGENTS.md content missing: %q", cap.got.System)
	}
}

func TestInstallAgentsMDDisabled(t *testing.T) {
	setAgentsMDFlag(t, "off")
	h := agent.NewHooks()
	if err := installAgentsMD(h, Deps{CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	cap := &captureLLM{}
	ag := &agent.Agent{LLM: cap, Registry: tools.New(), System: "base prompt", Hooks: h}
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if cap.got.System != "base prompt" {
		t.Fatalf("system changed with agents-md off: %q", cap.got.System)
	}
}

func TestInstallAgentsMDFailFast(t *testing.T) {
	setAgentsMDFlag(t, filepath.Join(t.TempDir(), "nope", agentsMDFile))
	if err := installAgentsMD(agent.NewHooks(), Deps{CWD: t.TempDir()}); err == nil {
		t.Fatal("explicit missing path should fail fast")
	}
}
