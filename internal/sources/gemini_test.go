package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
)

// These tests cover the Gemini adapter's replay and rendering rules on small records shaped after the
// types in gemini-cli's chatRecordingTypes.ts. They are not a format fixture: a real session must
// be written by the CLI itself (see TODO(fixture) in gemini.go).

const geminiSession = `{"sessionId":"s1","projectHash":"h","startTime":"2026-10-28T03:00:00.000Z","lastUpdated":"2026-10-28T03:00:00.000Z"}
{"id":"m1","timestamp":"2026-10-28T03:00:01.000Z","type":"user","content":[{"text":"/help"}]}
{"id":"m2","timestamp":"2026-10-28T03:00:02.000Z","type":"user","content":[{"text":"用 pnpm 跑测试"}]}
{"id":"m3","timestamp":"2026-10-28T03:00:03.000Z","type":"gemini","content":"先看配置","thoughts":[{"subject":"x","description":"secret thoughts"}],"toolCalls":[{"id":"c1","name":"run_shell_command","args":{"command":"pnpm test"},"status":"executing","timestamp":"2026-10-28T03:00:03.500Z"}]}
{"$patch":{"id":"m3","toolCalls":[{"id":"c1","result":[{"functionResponse":{"id":"c1","name":"run_shell_command","response":{"error":"connect ECONNREFUSED 127.0.0.1:5432"}}}]}]}}
{"id":"m4","timestamp":"2026-10-28T03:00:04.000Z","type":"error","content":"quota exceeded"}
{"id":"m5","timestamp":"2026-10-28T03:00:05.000Z","type":"user","content":"换个思路"}
{"id":"m6","timestamp":"2026-10-28T03:00:06.000Z","type":"gemini","content":"好","toolCalls":[{"id":"c2","name":"read_file","args":{"file_path":"a.ts"},"status":"success","result":[{"functionResponse":{"id":"c2","name":"read_file","response":{"output":"export const a = 1"}}}]}]}
`

func renderGemini(t *testing.T, content string) (*ir.Session, []ir.Line) {
	t.Helper()
	s, err := Gemini{}.OpenBytes(ir.SourceFile{Kind: ir.Gemini, Path: "/x/chats/session-1.jsonl"}, []byte(content), ScanOptions{})
	if err != nil || s == nil {
		t.Fatalf("%v %v", s, err)
	}
	return s, s.Render(0, -1)
}

func TestGeminiRendering(t *testing.T) {
	s, lines := renderGemini(t, geminiSession)
	if s.SessionID != "s1" {
		t.Fatal(s.SessionID)
	}
	want := `USER: 用 pnpm 跑测试
ASSISTANT: 先看配置
  → run_shell_command(command=pnpm test)
  ✗ run_shell_command failed: connect ECONNREFUSED 127.0.0.1:5432
  [assistant error: quota exceeded]
USER: 换个思路
ASSISTANT: 好
  → read_file(file_path=a.ts)
  ✓ read_file: export const a = 1`
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(ir.Text(lines), "secret thoughts") {
		t.Fatal("thoughts must be dropped")
	}
	turns := 0
	for _, l := range lines {
		if l.TurnStart {
			turns++
		}
	}
	if turns != 2 {
		t.Fatalf("turn starts: %d", turns)
	}
	if lines[3].Success == nil || *lines[3].Success || *lines[len(lines)-1].Success != true {
		t.Fatalf("success flags: %+v", lines)
	}
}

func TestGeminiRewindAndPatchRules(t *testing.T) {
	_, lines := renderGemini(t, geminiSession+`{"$rewindTo":"m5"}
{"id":"m7","timestamp":"2026-10-28T03:00:07.000Z","type":"user","content":"别的办法"}
{"$patch":{"removeIds":["m4"]}}
`)
	got := ir.Text(lines)
	if strings.Contains(got, "换个思路") || strings.Contains(got, "read_file") || !strings.Contains(got, "USER: 别的办法") {
		t.Fatalf("rewind must drop m5 and later:\n%s", got)
	}
	if strings.Contains(got, "quota exceeded") {
		t.Fatalf("removeIds must drop m4:\n%s", got)
	}
	// a message line with a known id replaces the message in place
	_, lines = renderGemini(t, geminiSession+`{"id":"m2","timestamp":"2026-10-28T03:00:02.000Z","type":"user","content":"用 npm"}`+"\n")
	if !strings.HasPrefix(ir.Text(lines), "USER: 用 npm\nASSISTANT: 先看配置") {
		t.Fatalf("replace in place:\n%s", ir.Text(lines))
	}
}

func TestGeminiIncrementalOffsetsAndLegacyFile(t *testing.T) {
	s, all := renderGemini(t, geminiSession)
	cut := all[1].Start // the first gemini message (all[0] is the user message)
	later := s.Render(cut, -1)
	if len(later) != len(all)-1 || later[0].Role != ir.RoleAssistant {
		t.Fatalf("from offset: %+v", later)
	}
	legacy := `{"sessionId":"s2","projectHash":"h","startTime":"t","summary":"Fix tests","messages":[{"id":"a","timestamp":"t","type":"user","content":"hi there"},{"id":"b","timestamp":"t","type":"gemini","content":"hello"}]}`
	ls, err := Gemini{}.OpenBytes(ir.SourceFile{Kind: ir.Gemini, Path: "/x/chats/session-2.json"}, []byte(legacy), ScanOptions{})
	if err != nil || ls == nil || ls.Title != "Fix tests" || ir.Text(ls.Render(0, -1)) != "USER: hi there\nASSISTANT: hello" {
		t.Fatalf("legacy: %+v %v", ls, err)
	}
}

func TestGeminiFindsCwdAndSubagents(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj1")
	chats := filepath.Join(proj, "chats")
	os.MkdirAll(filepath.Join(chats, "s1"), 0o755)
	os.WriteFile(filepath.Join(proj, ".project_root"), []byte("/home/dev/shop\n"), 0o644)
	os.WriteFile(filepath.Join(chats, "session-2026-10-28T03-00-aaaa.jsonl"), []byte(geminiSession), 0o644)
	os.WriteFile(filepath.Join(chats, "s1", "sub-1.jsonl"), []byte(`{"sessionId":"sub-1","projectHash":"h","kind":"subagent"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(chats, "notes.jsonl.bak"), []byte("x"), 0o644)
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.Gemini: {root}}}
	files := Gemini{}.Discover(cfg)
	if len(files) != 2 {
		t.Fatalf("%+v", files)
	}
	main, sub := files[0], files[1]
	if sub.ParentKey != main.Path || sub.AgentName != "sub-1" {
		t.Fatalf("subagent link: %+v", sub)
	}
	opened, err := Open(main)
	if err != nil || opened.Cwd != "/home/dev/shop" {
		t.Fatalf("cwd: %v %+v", err, opened)
	}
	if f, err := FileFor(main.Path); err != nil || f.Kind != ir.Gemini {
		t.Fatalf("sniff: %+v %v", f, err)
	}
}

// After a parallel batch the CLI stores every functionResponse of the batch in the `result` of each
// call; each call must be shown with its own answer only (seen in a session written with a real model).
func TestGeminiParallelCallsShowTheirOwnResult(t *testing.T) {
	batch := `[{"functionResponse":{"id":"c1","name":"read_file","response":{"output":"alpha"}}},{"functionResponse":{"id":"c2","name":"read_file","response":{"output":"beta"}}},{"functionResponse":{"id":"c3","name":"run_shell_command","response":{"output":"ok"}}}]`
	_, lines := renderGemini(t, `{"sessionId":"s1","projectHash":"h","startTime":"2026-10-28T03:00:00.000Z"}
{"id":"m1","timestamp":"2026-10-28T03:00:01.000Z","type":"user","content":[{"text":"look around"}]}
{"id":"m2","timestamp":"2026-10-28T03:00:02.000Z","type":"gemini","content":"","toolCalls":[{"id":"c1","name":"read_file","args":{"file_path":"a"},"status":"success","result":`+batch+`},{"id":"c2","name":"read_file","args":{"file_path":"b"},"status":"success","result":`+batch+`},{"id":"c3","name":"run_shell_command","args":{"command":"true"},"status":"success","result":`+batch+`}]}
`)
	want := `USER: look around
  → read_file(file_path=a)
  ✓ read_file: alpha
  → read_file(file_path=b)
  ✓ read_file: beta
  → run_shell_command(command=true)
  ✓ run_shell_command: ok`
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
