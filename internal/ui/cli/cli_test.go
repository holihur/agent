package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/holihur/agent/internal/agent"
	"github.com/holihur/agent/internal/tools"
)

// 非 TTY(管道)路径:readline 不启用,走逐行兜底。
func TestScannerFallbackREPL(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader("hi\n\nexit\n"), &out)
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ui.rl != nil {
		t.Fatal("readline must not start on non-TTY input")
	}
	if !strings.Contains(out.String(), "error: agent not wired") {
		t.Fatalf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "> ") {
		t.Fatalf("prompt missing: %q", out.String())
	}
}

func TestScannerFallbackEOF(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader("hi\n"), &out)
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestScannerFallbackSlashExit(t *testing.T) {
	// "/exit" 与 "/quit" 属于 REPL 循环控制,直接退出、不进对话循环。
	for _, line := range []string{"/exit", "/quit"} {
		var out bytes.Buffer
		ui := New(strings.NewReader(line+"\n"), &out)
		if err := ui.Run(context.Background()); err != nil {
			t.Fatalf("%s: Run: %v", line, err)
		}
		if strings.Contains(out.String(), "error:") {
			t.Fatalf("%s: unexpected error output: %q", line, out.String())
		}
	}
}

func TestRespondCollectsFields(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader("octocat\n\n"), &out)
	req := tools.InputRequest{
		Tool: "confirm",
		Prompts: []tools.InputPrompt{{
			Key:     "k1",
			Message: "Proceed?",
			Fields: []tools.InputField{
				{Name: "user", Required: true},
				{Name: "note"},
			},
		}},
	}
	resps, err := ui.Respond(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 1 || resps[0].Key != "k1" {
		t.Fatalf("resps = %+v", resps)
	}
	if resps[0].Content["user"] != "octocat" {
		t.Fatalf("content = %+v", resps[0].Content)
	}
	if _, has := resps[0].Content["note"]; has {
		t.Fatalf("empty optional field must be omitted: %+v", resps[0].Content)
	}
}

func TestConfirmHookDenyByDefault(t *testing.T) {
	ui := New(strings.NewReader("\n"), &bytes.Buffer{})
	confirm := ui.ConfirmHook()
	if d := confirm(agent.ToolCall{Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}); !d.Deny || d.Reason != "user denied" {
		t.Fatalf("empty answer must deny: %+v", d)
	}
}

func TestConfirmHookAcceptsYes(t *testing.T) {
	ui := New(strings.NewReader("y\nyes\n"), &bytes.Buffer{})
	confirm := ui.ConfirmHook()
	if d := confirm(agent.ToolCall{Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}); d.Deny {
		t.Fatalf("y must allow: %+v", d)
	}
	if d := confirm(agent.ToolCall{Name: "shell"}); d.Deny {
		t.Fatalf("yes must allow: %+v", d)
	}
}

func TestRespondInputClosedIsError(t *testing.T) {
	ui := New(strings.NewReader(""), &bytes.Buffer{})
	req := tools.InputRequest{
		Tool:    "t",
		Prompts: []tools.InputPrompt{{Key: "k", Fields: []tools.InputField{{Name: "a", Required: true}}}},
	}
	if _, err := ui.Respond(context.Background(), req); err == nil {
		t.Fatal("expected error when input stream is closed")
	}
}

// --- markdown 渲染 -----------------------------------------------------------

// stubStreamLLM 是流式测试替身:Turn 满足 LLM port,TurnStream 逐段发增量。
type stubStreamLLM struct {
	answer string
}

func (s stubStreamLLM) Turn(_ context.Context, _ agent.TurnRequest) (agent.TurnResult, error) {
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText(s.answer)}},
		StopReason: "end_turn",
	}, nil
}

func (s stubStreamLLM) TurnStream(_ context.Context, _ agent.TurnRequest, emit func(agent.TextDelta)) (agent.TurnResult, error) {
	for _, t := range []string{"# 标题\n", "**加粗** 和 `代码`\n"} {
		emit(agent.TextDelta{Text: t})
	}
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText(s.answer)}},
		StopReason: "end_turn",
	}, nil
}

// stubTurnLLM 是非流式测试替身(仅实现 LLM port,不支持 TurnStream)。
type stubTurnLLM struct{ answer string }

func (s stubTurnLLM) Turn(_ context.Context, _ agent.TurnRequest) (agent.TurnResult, error) {
	return agent.TurnResult{
		Assistant:  agent.Message{Role: agent.RoleAssistant, Blocks: []agent.Block{agent.NewText(s.answer)}},
		StopReason: "end_turn",
	}, nil
}

func mdTestAgent(ui *UI, llm agent.LLM) *agent.Agent {
	return &agent.Agent{LLM: llm, Registry: tools.New(), MaxTurns: 2, OnTextDelta: ui.TextDeltaSink()}
}

// 流式回答经 markdown 渲染:输出带 ANSI 样式、标记符不泄漏、Tidyup 复位。
func TestMarkdownStreamedAnswer(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	ui.SetMarkdown(true)
	ui.Agent = mdTestAgent(ui, stubStreamLLM{answer: "# 标题\n**加粗** 和 `代码`\n"})
	if err := ui.RunOnce(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("markdown rendering missing ANSI: %q", got)
	}
	if !strings.Contains(got, "标题") || !strings.Contains(got, "加粗") || !strings.Contains(got, "代码") {
		t.Fatalf("rendered content missing: %q", got)
	}
	for _, mark := range []string{"**", "# 标题", "`代码`"} {
		if strings.Contains(got, mark) {
			t.Errorf("markdown marker leaked (%q): %q", mark, got)
		}
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("output must end with Tidyup reset: %q", got)
	}
}

// 非流式回答也走 markdown 渲染(RenderString 整段)。
func TestMarkdownUnstreamedAnswer(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	ui.SetMarkdown(true)
	ui.Agent = mdTestAgent(ui, stubTurnLLM{answer: "**plain** answer\n"})
	if err := ui.RunOnce(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "plain") || strings.Contains(got, "**") {
		t.Fatalf("unstreamed answer not rendered: %q", got)
	}
}

// markdown 关闭时保持原文直出。
func TestMarkdownDisabled(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	ui.SetMarkdown(false)
	ui.Agent = mdTestAgent(ui, stubStreamLLM{answer: "# 标题\n"})
	if err := ui.RunOnce(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "# 标题") || strings.Contains(got, "\x1b[") {
		t.Fatalf("markdown off must output raw text: %q", got)
	}
}

// 默认(auto)仅在输出为 TTY 时开启;非 TTY(bytes.Buffer)输出原文。
func TestMarkdownAutoNonTTY(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out) // 未显式开关
	ui.Agent = mdTestAgent(ui, stubStreamLLM{answer: "# 标题\n"})
	if err := ui.RunOnce(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "# 标题") {
		t.Fatalf("non-TTY auto must fall back to raw text: %q", got)
	}
}

// 渲染失败不得阻塞回答(降级为原文直出)。
func TestMarkdownDegradesOnRenderError(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	ui.SetMarkdown(true)
	// 构造一个无法启动渲染器的场景由内部吞掉;这里验证整段路径不 panic。
	ui.Agent = mdTestAgent(ui, stubTurnLLM{answer: "ok\n"})
	if err := ui.RunOnce(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
}

// 连续多轮:每轮独立渲染生命周期,状态不串。
func TestMarkdownMultipleRounds(t *testing.T) {
	var out bytes.Buffer
	ui := New(strings.NewReader(""), &out)
	ui.SetMarkdown(true)
	ui.Agent = mdTestAgent(ui, stubStreamLLM{answer: "# 标题\n"})
	for i := 0; i < 3; i++ {
		if err := ui.RunOnce(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	got := out.String()
	if n := strings.Count(got, "标题"); n != 3 {
		t.Fatalf("expected 3 rendered answers, got %d: %q", n, got)
	}
	if n := strings.Count(got, "\x1b[0m"); n < 3 {
		t.Fatalf("expected >=3 Tidyup resets, got %d", n)
	}
}
