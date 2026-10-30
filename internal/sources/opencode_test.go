package sources

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/testutil"
)

func TestOpenCodeSessionsAndRendering(t *testing.T) {
	path := testutil.OpenCodeDB(t)
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.OpenCode: {path}}}
	files := OpenCode{}.Discover(cfg)
	if len(files) != 2 {
		t.Fatalf("%+v", files)
	}
	var main, sub ir.SourceFile
	for _, f := range files {
		if f.Inner == "ses_main" {
			main = f
		} else {
			sub = f
		}
	}
	if main.Key != path+"#ses_main" || main.MtimeMs != 1793000009000 || sub.ParentKey != main.Key || sub.AgentName != "Explore (@explore subagent)" {
		t.Fatalf("main %+v sub %+v", main, sub)
	}
	if got := (OpenCode{}).Discover(&config.Config{AgentDirs: cfg.AgentDirs, IncludeSubagents: boolPtr(false)}); len(got) != 1 {
		t.Fatalf("subagents off: %+v", got)
	}

	s, err := Open(main)
	if err != nil || s == nil || s.SessionID != "ses_main" || s.Cwd != "/home/dev/shop" || s.Title != "Fix order tests" {
		t.Fatalf("%v %+v", err, s)
	}
	lines := s.Render(0, -1)
	want := `USER: 用 pnpm 跑测试
[file: package.json]
ASSISTANT: 先跑一遍测试
  → bash(command=pnpm test description=run tests)
  ✗ bash failed: connect ECONNREFUSED 127.0.0.1:5432
  → read(filePath=/home/dev/shop/.env.test)
  ✓ read: DB_PORT=5433
  → bash(command=sleep 100)
  [assistant turn aborted by the user]`
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range []string{"hidden reasoning", "Called the Read tool", "compaction summary"} {
		if strings.Contains(ir.Text(lines), bad) {
			t.Fatalf("%q must not be rendered", bad)
		}
	}
	if !lines[0].TurnStart || lines[0].TS != "2026-10-26T07:33:21.000Z" {
		t.Fatalf("turn start / ts: %+v", lines[0])
	}
	if lines[3].Success == nil || *lines[3].Success || lines[5].Success == nil || !*lines[5].Success {
		t.Fatalf("success flags: %+v", lines)
	}

	// the stream is what mg prefilters: it must hold every searchable word
	raw, err := ReadFileInto(main, nil)
	if err != nil || !strings.Contains(string(raw), "ECONNREFUSED") || !strings.Contains(string(raw), "用 pnpm 跑测试") {
		t.Fatalf("raw: %v\n%s", err, raw)
	}
	// offsets are offsets in the stream: resuming from the second message line skips the first
	later := s.Render(lines[1].Start, -1)
	if len(later) != len(lines)-1 || later[0].Role != ir.RoleAssistant {
		t.Fatalf("resume: %+v", later)
	}
	if s.CompleteBytes != int64(len(raw)) {
		t.Fatalf("CompleteBytes %d vs stream %d", s.CompleteBytes, len(raw))
	}
}

func TestOpenCodeDatabaseGivenExplicitly(t *testing.T) {
	path := testutil.OpenCodeDB(t)
	files, err := FilesUnder(path)
	if err != nil || len(files) != 2 || files[0].Kind != ir.OpenCode {
		t.Fatalf("%+v %v", files, err)
	}
	dirFiles, err := FilesUnder(filepath.Dir(path))
	if err != nil || len(dirFiles) != 2 {
		t.Fatalf("directory: %+v %v", dirFiles, err)
	}
	other := filepath.Join(t.TempDir(), "other.db")
	db, _ := sql.Open("sqlite", other)
	db.Exec(`CREATE TABLE t (x)`)
	db.Close()
	if _, err := FilesUnder(other); err == nil {
		t.Fatal("a SQLite file without session tables is not an opencode database")
	}
}
