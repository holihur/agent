package skills

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/hook"
	"github.com/holihur/agent/internal/tools"
)

func writeSkill(t *testing.T, root, dir, content string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, skillFile)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoverSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "commit", "---\nname: commit\ndescription: Commit staged changes\n---\n\nDo the thing.\n")
	writeSkill(t, root, "plain", "just body, no frontmatter") // name 回落目录名

	// 无 SKILL.md 的目录与普通文件都应被跳过
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := discoverSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []skill{
		{Name: "commit", Description: "Commit staged changes", Body: "Do the thing."},
		{Name: "plain", Body: "just body, no frontmatter"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDiscoverSkillsMissingDir(t *testing.T) {
	got, err := discoverSkills(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dir should be silent, got %v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestDiscoverSkillsDuplicateName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "a", "---\nname: dup\n---\nx")
	writeSkill(t, root, "b", "---\nname: dup\n---\ny")
	if _, err := discoverSkills(root); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	cases := []struct {
		name, text, wantName, wantDesc, wantBody string
		wantNoMeta                               bool
	}{
		{
			name: "full", text: "---\nname: a\ndescription: \"does: things\"\n---\nBODY",
			wantName: "a", wantDesc: "does: things", wantBody: "BODY",
		},
		{
			name: "no-frontmatter", text: "hello", wantNoMeta: true, wantBody: "hello",
		},
		{
			name: "unterminated", text: "---\nname: a\nnot closed", wantNoMeta: true, wantBody: "---\nname: a\nnot closed",
		},
		{
			name: "blank-and-comments", text: "---\n# comment\n\nname: b\n---\n\nB",
			wantName: "b", wantBody: "B",
		},
		{
			name: "quoted-single", text: "---\ndescription: 'it''s fine'\n---\nB",
			wantDesc: "it''s fine", wantBody: "B", // 不做转义,只剥成对引号
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta, body := splitFrontmatter(c.text)
			if (meta == nil) != c.wantNoMeta {
				t.Fatalf("meta = %v, wantNoMeta = %v", meta, c.wantNoMeta)
			}
			if meta != nil {
				if meta["name"] != c.wantName || meta["description"] != c.wantDesc {
					t.Fatalf("meta = %v, want name %q desc %q", meta, c.wantName, c.wantDesc)
				}
			}
			if body != c.wantBody {
				t.Fatalf("body = %q, want %q", body, c.wantBody)
			}
		})
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

// setSkillsFlag 临时改写 -skills flag 全局,测试结束恢复。
func setSkillsFlag(t *testing.T, v string) {
	t.Helper()
	old := *skillsDir
	*skillsDir = v
	t.Cleanup(func() { *skillsDir = old })
}

// setupSkillsDir 在 root/.agents/skills 下造两个技能,返回 root。
func setupSkillsDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	skillsRoot := filepath.Join(root, ".agents", "skills")
	writeSkill(t, skillsRoot, "haiku", "---\nname: haiku\ndescription: always answer in haiku\n---\nUse 5-7-5.")
	writeSkill(t, skillsRoot, "plain", "plain body")
	return root
}

func TestInstallSkillsInjectsSystemAndTool(t *testing.T) {
	root := setupSkillsDir(t)
	setSkillsFlag(t, defaultSkillsDir) // 相对路径应按 CWD 解析

	lp := tools.NewLocal()
	h := agent.NewHooks()
	if err := installSkills(h, hook.Deps{CWD: root, Tools: lp}); err != nil {
		t.Fatal(err)
	}

	// 清单注入 system prompt(基础 prompt 在前)
	cap := &captureLLM{}
	ag := &agent.Agent{LLM: cap, Registry: tools.New(), System: "base prompt", Hooks: h}
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cap.got.System, "base prompt") {
		t.Fatalf("base prompt lost: %q", cap.got.System)
	}
	for _, frag := range []string{"# Available Skills", "- haiku: always answer in haiku", "- plain"} {
		if !strings.Contains(cap.got.System, frag) {
			t.Fatalf("catalog missing %q:\n%s", frag, cap.got.System)
		}
	}

	// skill 工具已注册到进程内工具平面
	defs, err := lp.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(defs, func(d tools.ToolDef) bool { return d.Name == "skill" }) {
		t.Fatalf("skill tool not registered: %+v", defs)
	}

	// 按名加载正文;未知名字报错
	res, err := lp.CallTool(context.Background(), "skill", []byte(`{"name":"haiku"}`))
	if err != nil || res.Text != "Use 5-7-5." {
		t.Fatalf("CallTool haiku = (%q, %v), want (Use 5-7-5., nil)", res.Text, err)
	}
	if _, err := lp.CallTool(context.Background(), "skill", []byte(`{"name":"nope"}`)); err == nil {
		t.Fatal("unknown skill should error")
	}
}

func TestInstallSkillsDisabled(t *testing.T) {
	root := setupSkillsDir(t)
	setSkillsFlag(t, "off")
	lp := tools.NewLocal()
	h := agent.NewHooks()
	if err := installSkills(h, hook.Deps{CWD: root, Tools: lp}); err != nil {
		t.Fatal(err)
	}
	cap := &captureLLM{}
	ag := &agent.Agent{LLM: cap, Registry: tools.New(), System: "base prompt", Hooks: h}
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if cap.got.System != "base prompt" {
		t.Fatalf("system changed with skills off: %q", cap.got.System)
	}
	if defs, _ := lp.ListTools(context.Background()); len(defs) != 0 {
		t.Fatalf("no tool should be registered when off, got %+v", defs)
	}
}

func TestInstallSkillsNoSkillsIsSilent(t *testing.T) {
	setSkillsFlag(t, defaultSkillsDir)
	lp := tools.NewLocal()
	h := agent.NewHooks()
	if err := installSkills(h, hook.Deps{CWD: t.TempDir(), Tools: lp}); err != nil {
		t.Fatal(err)
	}
	if defs, _ := lp.ListTools(context.Background()); len(defs) != 0 {
		t.Fatalf("no skills dir should register nothing, got %+v", defs)
	}
}

func TestInstallSkillsNeedsTools(t *testing.T) {
	root := setupSkillsDir(t)
	setSkillsFlag(t, defaultSkillsDir)
	if err := installSkills(agent.NewHooks(), hook.Deps{CWD: root}); err == nil {
		t.Fatal("skills present without Deps.Tools should fail fast")
	}
}
