package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/testutil"
)

// Tests on sessions the agents themselves wrote: Gemini CLI 0.66.0, Qwen Code 0.26.3 and opencode
// 1.19.5 ran scripted conversations (test/fixtures/gen/mock-model.mjs) in a throwaway HOME; each
// agent's gen.sh regenerates its fixture. Assertions avoid ids, times and directory listings, which
// change on every regeneration.

type rendered struct {
	file  ir.SourceFile
	sess  *ir.Session
	lines []ir.Line
	text  string
}

// renderAll opens every session an adapter finds under the fixture and indexes them by the first
// line of the conversation ("USER: …SCENARIO <name>" / "TASK for subagent: …").
func renderAll(t *testing.T, kind ir.SourceKind, rel string) map[string]*rendered {
	t.Helper()
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{kind: {testutil.Fixture(rel)}}}
	files := Get(kind).Discover(cfg)
	out := map[string]*rendered{}
	for _, f := range files {
		s, err := Open(f)
		if err != nil || s == nil {
			t.Fatalf("%s: %v %v", f.Path, s, err)
		}
		lines := s.Render(0, -1)
		if len(lines) == 0 {
			t.Fatalf("%s rendered nothing", f.Key)
		}
		first := lines[0].Text
		key := first
		if i := strings.Index(first, "SCENARIO "); i >= 0 {
			key = strings.Fields(first[i+len("SCENARIO "):])[0]
			key = strings.TrimSuffix(key, ":")
			if strings.HasPrefix(first, "TASK") {
				key = "subagent"
			}
		}
		out[key] = &rendered{f, s, lines, ir.Text(lines)}
	}
	return out
}

func mustContainInOrder(t *testing.T, label, text string, parts ...string) {
	t.Helper()
	rest := text
	for _, p := range parts {
		i := strings.Index(rest, p)
		if i < 0 {
			t.Fatalf("%s: %q not found (in order) in:\n%s", label, p, text)
		}
		rest = rest[i+len(p):]
	}
}

func assertNoNoise(t *testing.T, label, text string, noise ...string) {
	t.Helper()
	for _, n := range noise {
		if strings.Contains(text, n) {
			t.Fatalf("%s: %q must not be rendered:\n%s", label, n, text)
		}
	}
}

// assertConversationShape checks what is common to the three agents' scripted scenarios.
func assertConversationShape(t *testing.T, name string, r map[string]*rendered, shell, read string) {
	t.Helper()
	list, rec := r["list-files"], r["recover"]
	if list == nil || rec == nil {
		t.Fatalf("%s: sessions found: %v", name, keys(r))
	}
	mustContainInOrder(t, name+" list-files", list.text,
		"USER:", "SCENARIO list-files", "ASSISTANT: 我先看看目录里有什么。",
		"  → "+shell+"(command=ls -la)", "  ✓ "+shell+": ",
		"  → "+read+"(", "  ✓ "+read+": ",
		"ASSISTANT: 目录里有 notes.txt", "USER:", "SCENARIO followup", "ASSISTANT: 好的，不需要更多改动。")
	if !strings.Contains(list.text, "ECONNREFUSED 127.0.0.1:5432") {
		t.Fatalf("%s: the contents of notes.txt are missing:\n%s", name, list.text)
	}
	turns := 0
	for _, l := range list.lines {
		if l.TurnStart {
			turns++
		}
	}
	if turns != 2 {
		t.Fatalf("%s list-files: a resumed session has two turns, got %d", name, turns)
	}
	mustContainInOrder(t, name+" recover", rec.text,
		"USER:", "SCENARIO recover", "  → "+shell+"(command=cat missing.txt)",
		"  ✗ "+shell+" failed: ", "No such file or directory", "(exit 1)",
		"ASSISTANT: missing.txt 不存在", "  → "+shell+"(command=echo recovered)", "  ✓ "+shell+":", "ASSISTANT: 已恢复")
	var failed, ok int
	for _, l := range rec.lines {
		if l.Success != nil && !*l.Success {
			failed++
		} else if l.Success != nil {
			ok++
		}
	}
	if failed != 1 || ok != 1 {
		t.Fatalf("%s recover: want one failed and one successful result, got %d and %d", name, failed, ok)
	}
	for label, x := range r {
		assertNoNoise(t, name+" "+label, x.text, "<session_context>", "<untrusted_context>", "Process Group PGID", "Exit Code:", "Command: ", "hidden reasoning")
		if x.sess.Cwd != "/tmp/mem-grep-gen/work" {
			t.Errorf("%s %s: cwd %q", name, label, x.sess.Cwd)
		}
		if x.sess.SessionID == "" || x.sess.StartedAt == "" {
			t.Errorf("%s %s: no id or start time: %+v", name, label, x.sess)
		}
		// offsets: every line lies inside the session's bytes, and resuming from the second turn
		// renders only the second turn
		for _, l := range x.lines {
			if l.Start < 0 || l.End <= l.Start || l.End > x.sess.CompleteBytes {
				t.Fatalf("%s %s: offsets [%d,%d) outside %d bytes: %+v", name, label, l.Start, l.End, x.sess.CompleteBytes, l)
			}
		}
	}
	secondStart := int64(-1)
	n := 0
	for _, l := range list.lines {
		if l.TurnStart {
			if n++; n == 2 {
				secondStart = l.Start
			}
		}
	}
	later := list.sess.Render(secondStart, -1)
	if len(later) != 2 || !strings.HasPrefix(later[0].Text, "USER:") || !strings.Contains(later[1].Text, "不需要更多改动") {
		t.Fatalf("%s: resuming from the second turn: %+v", name, later)
	}
}

func keys(m map[string]*rendered) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestGeminiFixtureWrittenByTheCLI(t *testing.T) {
	r := renderAll(t, ir.Gemini, "gemini/tmp")
	assertConversationShape(t, "gemini", r, "run_shell_command", "read_file")
	del, sub := r["delegate"], r["subagent"]
	if del == nil || sub == nil {
		t.Fatalf("sessions found: %v", keys(r))
	}
	mustContainInOrder(t, "gemini delegate", del.text, "SCENARIO delegate", "  → invoke_agent(", "  ✓ invoke_agent: ", "ASSISTANT: 子任务完成了。")
	if sub.file.ParentKey != del.file.Path || sub.file.AgentName == "" {
		t.Fatalf("the subagent file must link to its parent: %+v (parent %s)", sub.file, del.file.Path)
	}
	if !strings.HasPrefix(sub.text, "TASK for subagent: SCENARIO followup") || strings.Contains(sub.text, "You have stopped calling tools") {
		t.Fatalf("subagent:\n%s", sub.text)
	}
	if sub.lines[0].Role != ir.RoleTask || !sub.lines[0].TurnStart {
		t.Fatalf("%+v", sub.lines[0])
	}
}

func TestQwenFixtureWrittenByTheCLI(t *testing.T) {
	r := renderAll(t, ir.Qwen, "qwen/projects")
	assertConversationShape(t, "qwen", r, "run_shell_command", "read_file")
}

func TestOpenCodeFixtureWrittenByTheCLI(t *testing.T) {
	r := renderAll(t, ir.OpenCode, "opencode/opencode.db")
	assertConversationShape(t, "opencode", r, "bash", "read")
	del, sub := r["delegate"], r["subagent"]
	if del == nil || sub == nil {
		t.Fatalf("sessions found: %v", keys(r))
	}
	mustContainInOrder(t, "opencode delegate", del.text, "SCENARIO delegate", "  → task(", "  ✓ task: ", "ASSISTANT: 子任务完成了。")
	if sub.file.ParentKey != del.file.Key || sub.file.AgentName == "" || sub.lines[0].Role != ir.RoleTask {
		t.Fatalf("subagent: %+v %+v", sub.file, sub.lines[0])
	}
	// the stream built from the database is what OpenBytes parsed: its size is the session's size
	raw, err := ReadFileInto(del.file, nil)
	if err != nil || int64(len(raw)) != del.sess.CompleteBytes {
		t.Fatalf("stream %d bytes vs %d (%v)", len(raw), del.sess.CompleteBytes, err)
	}
}

func TestAgentsAreToldApartOnRealFiles(t *testing.T) {
	check := func(rel string, want ir.SourceKind) {
		t.Helper()
		var seen int
		_ = filepath.WalkDir(testutil.Fixture(rel), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			f, err := FileFor(p)
			if err != nil || f.Kind != want {
				t.Errorf("%s sniffed as %v (%v), want %s", p, f.Kind, err, want)
			}
			seen++
			return nil
		})
		if seen == 0 {
			t.Fatalf("no files under %s", rel)
		}
	}
	check("gemini/tmp", ir.Gemini)
	check("qwen/projects", ir.Qwen)
	check("sessions", ir.OMP)
}
