package sources

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/testutil"
)

// zcodeFiles discovers the test storage root and sorts what it finds by kind.
func zcodeFiles(t *testing.T, root string, cfg *config.Config) (sessions map[string]ir.SourceFile, memories map[string]ir.SourceFile) {
	t.Helper()
	sessions, memories = map[string]ir.SourceFile{}, map[string]ir.SourceFile{}
	for _, f := range (ZCode{}).Discover(cfg) {
		if zcodeIsMemory(f) {
			rel, _ := filepath.Rel(filepath.Join(root, "memories", "projects"), f.Path)
			memories[filepath.ToSlash(rel)] = f
		} else {
			sessions[zcodeSessionID(f.Inner)] = f
		}
	}
	return sessions, memories
}

func TestZCodeSessionsAndRendering(t *testing.T) {
	root := testutil.ZCodeHome(t)
	db := filepath.Join(root, "db", "db.sqlite")
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.ZCode: {root}}}
	sessions, memories := zcodeFiles(t, root, cfg)
	if len(sessions) != 3 || len(memories) != 3 {
		t.Fatalf("sessions %+v\nmemories %+v", sessions, memories)
	}
	main, sub, fork := sessions["11111111-aaaa-4bbb-8ccc-000000000001"], sessions["22222222-aaaa-4bbb-8ccc-000000000002"], sessions["33333333-aaaa-4bbb-8ccc-000000000003"]
	if main.Key != db+"#sess_11111111-aaaa-4bbb-8ccc-000000000001" || main.MtimeMs != 1793000020000 || main.ParentKey != "" {
		t.Fatalf("main %+v", main)
	}
	if sub.ParentKey != main.Key || sub.AgentName != "zcode-Explore" {
		t.Fatalf("sub %+v", sub)
	}
	if fork.Path == "" || fork.ParentKey != "" {
		t.Fatalf("a fork is the user's own conversation, not a subagent: %+v", fork)
	}
	// subagents off: the subagent session goes, the fork stays
	off, _ := zcodeFiles(t, root, &config.Config{AgentDirs: cfg.AgentDirs, IncludeSubagents: boolPtr(false)})
	if _, hasFork := off["33333333-aaaa-4bbb-8ccc-000000000003"]; len(off) != 2 || !hasFork {
		t.Fatalf("subagents off: %+v", off)
	}

	s, err := Open(main)
	if err != nil || s == nil || s.SessionID != "11111111-aaaa-4bbb-8ccc-000000000001" || s.Cwd != "/home/dev/shop" || s.Title != "Fix order tests" {
		t.Fatalf("%v %+v", err, s)
	}
	lines := s.Render(0, -1)
	want := `USER: 用 pnpm 跑测试
[file: package.json]
ASSISTANT: 先跑一遍测试
  → Bash(command=pnpm test description=run tests)
  ✗ Bash failed: Exit code 1
connect ECONNREFUSED 127.0.0.1:5432
  → Read(file_path=/home/dev/shop/.env.test)
  ✓ Read: DB_PORT=5433
  → Bash(command=sleep 100)
  [assistant turn aborted by the user]
USER: 把数据库换成 mysql
ASSISTANT: 好的，改成 mysql。
[branch: the user rolled back earlier turns; what came after them was abandoned]
USER: 测试库的端口是 5433
  → Edit(file_path=/home/dev/shop/.env.test (+new_string,old_string))
  ✗ Edit failed: String to replace not found in file.
ASSISTANT: 端口已经是 5433，测试通过了。
  [assistant error: [1308] usage limit reached]`
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if !lines[0].TurnStart || lines[0].TS != "2026-10-26T07:33:21.000Z" || lines[0].EntryID != "msg_1" {
		t.Fatalf("turn start / ts / id: %+v", lines[0])
	}
	if lines[3].Success == nil || *lines[3].Success || lines[5].Success == nil || !*lines[5].Success {
		t.Fatalf("success flags: %+v %+v", lines[3], lines[5])
	}
	turns := 0
	for _, l := range lines {
		if l.TurnStart {
			turns++
		}
	}
	if turns != 4 { // three user messages and the branch note
		t.Fatalf("%d turns", turns)
	}

	// the stream is what mg prefilters: it holds every searchable word, and nothing that is
	// never rendered (thinking, the file snapshots in tool metadata)
	raw, err := ReadFileInto(main, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"ECONNREFUSED", "用 pnpm 跑测试", "aborted", "package.json"} {
		if !strings.Contains(string(raw), word) {
			t.Errorf("stream lacks %q", word)
		}
	}
	for _, word := range []string{"hidden reasoning", "SNAPSHOT_ONLY_IN_METADATA", "step-finish"} {
		if strings.Contains(string(raw), word) {
			t.Errorf("stream holds %q", word)
		}
	}
	if s.CompleteBytes != int64(len(raw)) {
		t.Fatalf("CompleteBytes %d vs stream %d", s.CompleteBytes, len(raw))
	}
	// offsets are offsets in the stream: resuming after the first message skips its lines
	later := s.Render(lines[1].Start, -1)
	if len(later) != len(lines)-1 || later[0].Role != ir.RoleAssistant {
		t.Fatalf("resume: %+v", later)
	}

	ss, err := Open(sub)
	if err != nil || ss == nil || ss.SessionID != "22222222-aaaa-4bbb-8ccc-000000000002" {
		t.Fatalf("%v %+v", err, ss)
	}
	if got := ir.Text(ss.Lines(0)); got != "TASK for subagent \"zcode-Explore\": find the test db config\nASSISTANT: It is in .env.test" {
		t.Fatalf("subagent:\n%s", got)
	}
	if l := ss.Lines(0)[0]; l.Role != ir.RoleTask || !l.TurnStart {
		t.Fatalf("task line: %+v", l)
	}
}

func TestZCodeMemories(t *testing.T) {
	root := testutil.ZCodeHome(t)
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.ZCode: {root}}}
	_, memories := zcodeFiles(t, root, cfg)
	project := testutil.ZCodeMemoryProject(testutil.ZCodeWorkspace)
	fb, ok := memories[project+"/memory/feedback_test_db.md"]
	if !ok || fb.Key != fb.Path || !strings.HasPrefix(fb.Inner, "mem-") || len(fb.Inner) != 16 || fb.ParentKey != "" {
		t.Fatalf("%+v in %+v", fb, memories)
	}
	s, err := Open(fb)
	if err != nil || s == nil {
		t.Fatalf("%v %+v", err, s)
	}
	// the project comes from the hash in the directory name: the main session's workspace identity
	if s.SessionID != fb.Inner || s.Cwd != "/home/dev/shop" || s.Title != "tests run against the real database on port 5433" || s.StartedAt == "" {
		t.Fatalf("%+v", s)
	}
	lines := s.Lines(0)
	want := `MEMORY feedback_test_db.md [feedback]: tests run against the real database on port 5433
Never mock the database in integration tests.
**Why:** a mocked migration passed while production failed.
**How to apply:** start postgres on 5433 first; the token is [REDACTED].`
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for i, l := range lines {
		if l.Role != ir.RoleMemory || l.TurnStart != (i == 0) || l.TS != s.StartedAt {
			t.Fatalf("line %d: %+v", i, l)
		}
	}
	raw, err := ReadFileInto(fb, nil)
	if err != nil || !strings.Contains(string(raw), "Never mock the database") || !strings.Contains(string(raw), "feedback_test_db.md") {
		t.Fatalf("raw: %v\n%s", err, raw)
	}
	if strings.Contains(string(raw), "node_type") {
		t.Fatalf("front matter keys are not rendered, so they stay out of the stream:\n%s", raw)
	}
	if s.CompleteBytes != int64(len(raw)) {
		t.Fatalf("CompleteBytes %d vs stream %d", s.CompleteBytes, len(raw))
	}

	// the index file has no front matter
	idx, err := Open(memories[project+"/memory/MEMORY.md"])
	if err != nil || idx == nil || idx.Cwd != "/home/dev/shop" {
		t.Fatalf("%v %+v", err, idx)
	}
	if got := ir.Text(idx.Lines(0)); got != "MEMORY MEMORY.md\n- [Test database](feedback_test_db.md) — tests run against the real database on port 5433" {
		t.Fatalf("index:\n%s", got)
	}

	// no session has this directory's hash: the memory's origin session names the project; a
	// folded description is joined, and the path below memory/ is kept
	nested, err := Open(memories["project-0000000000000000/memory/notes/user_role.md"])
	if err != nil || nested == nil || nested.Cwd != "/home/dev/shop" {
		t.Fatalf("%v %+v", err, nested)
	}
	if got := ir.Text(nested.Lines(0)); got != "MEMORY notes/user_role.md [user]: the user maintains the shop backend\nPrefers pnpm over npm." {
		t.Fatalf("nested:\n%s", got)
	}
	if nested.SessionID == s.SessionID || nested.SessionID == idx.SessionID {
		t.Fatalf("memory ids collide: %s %s %s", nested.SessionID, s.SessionID, idx.SessionID)
	}
}

func TestZCodeGivenExplicitly(t *testing.T) {
	root := testutil.ZCodeHome(t)
	db := filepath.Join(root, "db", "db.sqlite")
	// the database: every session that is not deleted, and never mistaken for opencode's
	files, err := FilesUnder(db)
	if err != nil || len(files) != 3 {
		t.Fatalf("%+v %v", files, err)
	}
	for _, f := range files {
		if f.Kind != ir.ZCode {
			t.Fatalf("%+v", f)
		}
	}
	if _, ok := (ZCode{}).SessionsIn(testutil.OpenCodeDB(t)); ok {
		t.Fatal("an opencode database is not a ZCode database")
	}
	// the storage root: sessions and memories
	all, err := FilesUnder(root)
	if err != nil || len(all) != 6 {
		t.Fatalf("%+v %v", all, err)
	}
	// one memory file
	path := filepath.Join(root, "memories", "projects", "project-0000000000000000", "memory", "notes", "user_role.md")
	one, err := FilesUnder(path)
	if err != nil || len(one) != 1 || one[0].Kind != ir.ZCode || !zcodeIsMemory(one[0]) {
		t.Fatalf("%+v %v", one, err)
	}
	if s, err := Open(one[0]); err != nil || s == nil || len(s.Lines(0)) != 2 {
		t.Fatalf("%v %+v", err, s)
	}
	// the database given as the root finds the memories beside it
	sessions, memories := zcodeFiles(t, root, &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.ZCode: {db}}})
	if len(sessions) != 3 || len(memories) != 3 {
		t.Fatalf("%+v %+v", sessions, memories)
	}
	// any other Markdown file is not a memory
	if _, err := FileFor(testutil.Fixture("../../README.md")); err == nil {
		t.Fatal("README.md is not a session file")
	}
}

func TestZCodePolicyOnRecordsWithoutSemantics(t *testing.T) {
	text := func(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }
	for _, c := range []struct {
		name  string
		info  map[string]any
		parts []map[string]any
		want  int
	}{
		{"plain user", map[string]any{"role": "user"}, text("hi"), zcodeUser},
		{"plain assistant", map[string]any{"role": "assistant"}, text("hello"), zcodeAssistant},
		{"model-only", map[string]any{"role": "user", "visibility": "model-only"}, text("x"), zcodeHidden},
		{"synthetic source", map[string]any{"role": "user", "source": "todo_reminder"}, text("x"), zcodeHidden},
		{"synthetic part", map[string]any{"role": "user"}, []map[string]any{{"type": "text", "text": "x", "synthetic": true}}, zcodeHidden},
		{"model-only part", map[string]any{"role": "user"}, []map[string]any{{"type": "text", "text": "x", "metadata": map[string]any{"visibility": "model-only"}}}, zcodeHidden},
		{"goal reminder", map[string]any{"role": "user"}, text(`<system-reminder source="goal-continuation">go on`), zcodeHidden},
		{"rewind notice", map[string]any{"role": "user"}, text("Conversation rewind applied."), zcodeHidden},
		{"timeline part", map[string]any{"role": "assistant"}, []map[string]any{{"type": "timeline"}}, zcodeHidden},
		{"compaction summary", map[string]any{"role": "assistant", "summary": true}, text("summary"), zcodeHidden},
		{"hidden copy of an assistant reply", map[string]any{"role": "assistant", "semantics": map[string]any{"origin": "agent_runtime", "kind": "assistant_response", "uiVisibility": "hidden", "providerVisibility": "visible", "transcriptVisibility": "visible"}}, text("x"), zcodeHidden},
		{"workflow launch", map[string]any{"role": "user", "synthetic": true, "semantics": map[string]any{"origin": "real_user", "kind": "user_prompt", "providerVisibility": "visible"}}, text("x"), zcodeHidden},
	} {
		if got := zcodePolicy(c.info, c.parts); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMemoryFrontMatter(t *testing.T) {
	meta, body := memoryFrontMatter("\xef\xbb\xbf---\r\nname: 'a b'\r\ndescription: |\r\n  one\r\n  two\r\nmetadata:\r\n  type: project\r\n---\r\nbody\r\n")
	if meta["name"] != "a b" || meta["description"] != "one two" || meta["type"] != "project" || body != "body\n" {
		t.Fatalf("%q %q", meta, body)
	}
	if meta, body := memoryFrontMatter("# no front matter\n---\nx"); meta != nil || body != "# no front matter\n---\nx" {
		t.Fatalf("%q %q", meta, body)
	}
	if meta, body := memoryFrontMatter("---\nname: unterminated\n"); meta != nil || !strings.HasPrefix(body, "---") {
		t.Fatalf("%q %q", meta, body)
	}
}
