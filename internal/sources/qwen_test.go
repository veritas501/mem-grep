package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
)

// Rule tests on small records shaped after ChatRecord in qwen-code's chatRecordingService.ts; not a
// format fixture (see TODO(fixture) in qwen.go).

const qwenSession = `{"uuid":"u0","parentUuid":null,"sessionId":"q1","timestamp":"2026-10-28T03:00:00.000Z","type":"system","subtype":"slash_command","cwd":"/home/dev/shop","version":"1.0.0","systemPayload":{"phase":"invocation","rawCommand":"/model"}}
{"uuid":"u1","parentUuid":"u0","sessionId":"q1","timestamp":"2026-10-28T03:00:01.000Z","type":"user","cwd":"/home/dev/shop","version":"1.0.0","message":{"role":"user","parts":[{"text":"用 pnpm 跑测试"}]}}
{"uuid":"u2","parentUuid":"u1","sessionId":"q1","timestamp":"2026-10-28T03:00:02.000Z","type":"assistant","cwd":"/home/dev/shop","version":"1.0.0","model":"qwen3-coder","message":{"role":"model","parts":[{"text":"hidden reasoning","thought":true},{"text":"先跑一遍"},{"functionCall":{"id":"call_1","name":"run_shell_command","args":{"command":"pnpm test"}}}]}}
{"uuid":"u3","parentUuid":"u2","sessionId":"q1","timestamp":"2026-10-28T03:00:03.000Z","type":"tool_result","cwd":"/home/dev/shop","version":"1.0.0","message":{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"run_shell_command","response":{"error":"connect ECONNREFUSED 127.0.0.1:5432"}}}]},"toolCallResult":{"callId":"call_1","status":"error"}}
{"uuid":"u4","parentUuid":"u3","sessionId":"q1","timestamp":"2026-10-28T03:00:04.000Z","type":"user","subtype":"notification","provenance":"system","cwd":"/home/dev/shop","version":"1.0.0","message":{"role":"user","parts":[{"text":"<task-notification>done</task-notification>"}]}}
{"uuid":"u5","parentUuid":"u4","sessionId":"q1","timestamp":"2026-10-28T03:00:05.000Z","type":"assistant","cwd":"/home/dev/shop","version":"1.0.0","message":{"role":"model","parts":[{"text":"需要先启动数据库。"}]}}
{"uuid":"u6","parentUuid":"u5","sessionId":"q1","timestamp":"2026-10-28T03:00:06.000Z","type":"system","subtype":"custom_title","cwd":"/home/dev/shop","version":"1.0.0","systemPayload":{"customTitle":"Fix order tests"}}
{"uuid":"u7","parentUuid":"u6","sessionId":"q1","timestamp":"2026-10-28T03:00:07.000Z","type":"user","subtype":"mid_turn_user_message","cwd":"/home/dev/shop","version":"1.0.0","message":{"role":"user","parts":[{"text":"顺便看下 .env.test"}]}}
`

func TestQwenRendering(t *testing.T) {
	s, err := Qwen{}.OpenBytes(ir.SourceFile{Kind: ir.Qwen, Path: "/x/chats/q1.jsonl"}, []byte(qwenSession), ScanOptions{})
	if err != nil || s == nil {
		t.Fatalf("%v %v", s, err)
	}
	if s.SessionID != "q1" || s.Cwd != "/home/dev/shop" || s.Title != "Fix order tests" {
		t.Fatalf("%+v", s)
	}
	lines := s.Render(0, -1)
	want := `USER: 用 pnpm 跑测试
ASSISTANT: 先跑一遍
  → run_shell_command(command=pnpm test)
  ✗ run_shell_command failed: connect ECONNREFUSED 127.0.0.1:5432
ASSISTANT: 需要先启动数据库。
USER: 顺便看下 .env.test`
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(ir.Text(lines), "hidden reasoning") || strings.Contains(ir.Text(lines), "task-notification") {
		t.Fatal("thoughts and CLI notifications must be dropped")
	}
	starts := 0
	for _, l := range lines {
		if l.TurnStart {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("a mid-turn message does not open a turn; turn starts: %d", starts)
	}
	if lines[3].Success == nil || *lines[3].Success {
		t.Fatalf("failed result: %+v", lines[3])
	}
}

func TestQwenAndClaudeAreToldApart(t *testing.T) {
	qwen := []byte(qwenSession)
	claude := []byte(`{"type":"user","uuid":"a","parentUuid":null,"sessionId":"c1","cwd":"/x","message":{"role":"user","content":"hi"}}` + "\n")
	if !(Qwen{}).Sniff("", qwen) || (Claude{}).Sniff("", qwen) {
		t.Fatal("a Qwen transcript must not be claimed by the Claude adapter")
	}
	if (Qwen{}).Sniff("", claude) || !(Claude{}).Sniff("", claude) {
		t.Fatal("a Claude transcript must not be claimed by the Qwen adapter")
	}
	if (Gemini{}).Sniff("", qwen) || (Gemini{}).Sniff("", claude) {
		t.Fatal("gemini")
	}
}

func TestQwenDiscoversChatsAndSubagents(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-home-dev-shop")
	os.MkdirAll(filepath.Join(proj, "chats"), 0o755)
	os.MkdirAll(filepath.Join(proj, "subagents", "q1"), 0o755)
	os.WriteFile(filepath.Join(proj, "chats", "q1.jsonl"), []byte(qwenSession), 0o644)
	os.WriteFile(filepath.Join(proj, "chats", "q1.runtime.json"), []byte(`{}`), 0o644)
	sub := strings.ReplaceAll(strings.Split(qwenSession, "\n")[1], `"u1"`, `"s1"`)
	os.WriteFile(filepath.Join(proj, "subagents", "q1", "agent-explore-7f3c.jsonl"), []byte(sub+"\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "subagents", "q1", "agent-explore-7f3c.meta.json"), []byte(`{"agentType":"Explore"}`), 0o644)
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.Qwen: {root}}}
	files := Qwen{}.Discover(cfg)
	if len(files) != 2 {
		t.Fatalf("%+v", files)
	}
	var main, agent ir.SourceFile
	for _, f := range files {
		if f.ParentKey == "" {
			main = f
		} else {
			agent = f
		}
	}
	if agent.ParentKey != main.Path || agent.AgentName != "Explore" {
		t.Fatalf("subagent: %+v (main %+v)", agent, main)
	}
	cfg2 := &config.Config{AgentDirs: cfg.AgentDirs, IncludeSubagents: boolPtr(false)}
	if got := (Qwen{}).Discover(cfg2); len(got) != 1 {
		t.Fatalf("subagents off: %+v", got)
	}
}

func boolPtr(b bool) *bool { return &b }
