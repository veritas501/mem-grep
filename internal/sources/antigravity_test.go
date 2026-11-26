package sources

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/testutil"
)

// These tests cover the Antigravity adapter's rules on a storage root built from the shapes agy
// writes (testutil.AntigravityHome). They are not a format fixture.

func antigravityTranscriptOf(root, id string) string {
	return filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript_full.jsonl")
}

func TestAntigravityRendering(t *testing.T) {
	root := testutil.AntigravityHome(t)
	f, err := FileFor(antigravityTranscriptOf(root, testutil.AntigravityMain))
	if err != nil || f.Kind != ir.Antigravity {
		t.Fatalf("%+v %v", f, err)
	}
	s, err := Open(f)
	if err != nil || s == nil {
		t.Fatalf("%v %v", s, err)
	}
	if s.SessionID != testutil.AntigravityMain || s.Cwd != "/home/dev/shop" || s.Title != "Run The Tests" || s.StartedAt != "2026-11-24T11:01:27Z" {
		t.Fatalf("%+v", s)
	}
	lines := s.Lines(0)
	want := `USER: 用 pnpm 跑测试
ASSISTANT: 先看目录。
  → run_command(CommandLine=pnpm test (+WaitMsBeforeAsync,toolSummary))
  → read_file(Path=/home/dev/shop/notes.txt (+StartLine))
  ✗ run_command failed: Command exited with code 1.

Command output:
Error: connect ECONNREFUSED 127.0.0.1:5432
  ✓ read_file: Showing lines 1-1 of 1 1: start test-db before pnpm test
  → edit_file(Path=/home/dev/shop/a.ts (+Replacement,Target))
  ✗ edit_file failed: target content not found in the file
  [assistant error: response was blocked by content safety filters]
[branch: the user rolled back earlier turns; what came after them was abandoned]
USER: 换个思路，先启动 test-db
  → run_command(CommandLine=docker start test-db (+WaitMsBeforeAsync))
  → invoke_subagent(TypeName=research Prompt=audit the mysql config (+Role))
  ✓ run_command: Tool is running as a background task with task id: ` + testutil.AntigravityMain + `/task-9
  ✓ invoke_subagent: Created the following subagent: ` + testutil.AntigravitySubagent + `
  [command output: Task id "task-9" finished with result:
Command exited with code 0.

Command output:
test-db started on port 5433]
[message from another agent: the mysql config uses sqlite in tests]
ASSISTANT: test-db 已启动，测试通过。`
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, dropped := range []string{"secret thoughts", "Reminder", "Retries remaining", "compaction", "ADDITIONAL_METADATA", "Model Selection", "Created At"} {
		if strings.Contains(ir.Text(lines), dropped) {
			t.Fatalf("%q must not be rendered", dropped)
		}
	}
	turns := 0
	for _, l := range lines {
		if l.TurnStart {
			turns++
		}
	}
	if turns != 3 {
		t.Fatalf("turn starts: %d", turns)
	}
	if lines[4].Success == nil || *lines[4].Success || lines[5].Success == nil || !*lines[5].Success {
		t.Fatalf("success flags: %+v %+v", lines[4], lines[5])
	}
}

func TestAntigravitySubagentAndDiscover(t *testing.T) {
	root := testutil.AntigravityHome(t)
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.Antigravity: {root}}}
	files := Antigravity{}.Discover(cfg)
	if len(files) != 2 {
		t.Fatalf("%+v", files)
	}
	var sub ir.SourceFile
	for _, f := range files {
		if f.ParentKey != "" {
			sub = f
		}
	}
	if sub.ParentKey != antigravityTranscriptOf(root, testutil.AntigravityMain) || sub.AgentName != "research" {
		t.Fatalf("%+v", sub)
	}
	s, err := Open(sub)
	if err != nil || s == nil {
		t.Fatal(err)
	}
	want := `TASK for subagent: audit the mysql config
  → send_message(Recipient=` + testutil.AntigravityMain + ` Message=the mysql config uses sqlite in tests)
  ✓ send_message: Message sent to "` + testutil.AntigravityMain + `"`
	if got := ir.Text(s.Lines(0)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	off := false
	cfg.IncludeSubagents = &off
	if files := (Antigravity{}).Discover(cfg); len(files) != 1 || files[0].ParentKey != "" {
		t.Fatalf("%+v", files)
	}

	// a directory given explicitly: only the full transcripts, not the cut copy or the chunks
	under, err := FilesUnder(root)
	if err != nil || len(under) != 2 {
		t.Fatalf("%+v %v", under, err)
	}
	for _, f := range under {
		if f.Kind != ir.Antigravity || filepath.Base(f.Path) != "transcript_full.jsonl" {
			t.Fatalf("%+v", f)
		}
	}
}

func TestAntigravityWorkspace(t *testing.T) {
	for in, want := range map[string]string{
		`["file:///home/dev/shop"]`:                    "/home/dev/shop",
		`["file:///home/dev/my%20shop","file:///tmp"]`: "/home/dev/my shop",
		`[]`: "",
		``:   "",
		`["https://example.com/x","file:///srv/app/a b"]`: "/srv/app/a b",
		`["file:///C:/Users/dev/shop"]`:                   filepath.FromSlash("C:/Users/dev/shop"),
	} {
		if got := antigravityWorkspace(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}
