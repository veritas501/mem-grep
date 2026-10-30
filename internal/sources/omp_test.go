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

func mainFixture(t *testing.T) (string, *OmpSession) {
	t.Helper()
	dir := testutil.Sessions(t)
	p, err := ParseOmp(filepath.Join(dir, testutil.MainRel))
	if err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func TestParseOmpFixture(t *testing.T) {
	_, p := mainFixture(t)
	if str(p.Header, "cwd") != testutil.CWD || p.Header["version"].(float64) != 3 {
		t.Fatalf("header: %v", p.Header)
	}
	if len(p.Entries) != 14 || p.BadLines != 0 || p.CompleteBytes != p.Size {
		t.Fatalf("entries=%d bad=%d complete=%d size=%d", len(p.Entries), p.BadLines, p.CompleteBytes, p.Size)
	}
}

func TestRenderOmp(t *testing.T) {
	_, p := mainFixture(t)
	text := ir.Text(RenderOmp(p, 0, -1))
	for _, want := range []string{
		"USER: 帮我把订单服务的单元测试跑通",
		"✗ bash failed: Error: connect ECONNREFUSED",
		"[branch: rewound to earlier point #1a38e318",
		"USER SHELL $ git status --short (exit 0)",
		"[user ran /clear",
		"[assistant turn aborted by the user]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"secret thoughts", "sk-ant-api03"} {
		if strings.Contains(text, bad) {
			t.Errorf("must not contain %q (thinking / secret)", bad)
		}
	}
}

func TestGroupTurns(t *testing.T) {
	_, p := mainFixture(t)
	turns := ir.GroupTurns(RenderOmp(p, 0, -1), 3000)
	if len(turns) != 5 || !strings.HasPrefix(turns[0].Text, "USER: 帮我") {
		t.Fatalf("turns=%d first=%q", len(turns), turns[0].Text)
	}
}

func TestRenderFromOffsetKeepsBranchState(t *testing.T) {
	_, p := mainFixture(t)
	var off int64 = -1
	for _, e := range p.Entries {
		if e.typ() == "branch_summary" {
			off = e.Start
			break
		}
	}
	lines := RenderOmp(p, off, -1)
	if !strings.Contains(lines[0].Text, "[branch: rewound") || strings.Contains(ir.Text(lines), "帮我把订单服务") {
		t.Fatalf("got %q", ir.Text(lines))
	}
}

func TestPartialLastLineIgnored(t *testing.T) {
	dir, before := mainFixture(t)
	f := filepath.Join(dir, testutil.MainRel)
	fh, _ := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(`{"type":"message","id":"zz","parentId":"25ed59f1","timestamp":"2026-10-28T04:00:00Z","message":{"role":"user","content":"半`)
	fh.Close()
	after, _ := ParseOmp(f)
	if len(after.Entries) != len(before.Entries) || after.CompleteBytes != before.Size {
		t.Fatalf("entries %d→%d complete=%d", len(before.Entries), len(after.Entries), after.CompleteBytes)
	}
}

func TestLegacyV1Session(t *testing.T) {
	f := filepath.Join(t.TempDir(), "v1.jsonl")
	os.WriteFile(f, []byte(`{"type":"session","id":"old","timestamp":"2024-01-01T00:00:00Z","cwd":"/x"}
{"type":"message","timestamp":"2024-01-01T00:00:01Z","message":{"role":"user","content":"hello"}}
{"type":"message","timestamp":"2024-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}
`), 0o644)
	p, _ := ParseOmp(f)
	if got := ir.Text(RenderOmp(p, 0, -1)); got != "USER: hello\nASSISTANT: hi" {
		t.Fatalf("got %q", got)
	}
}

func TestDiscoverSubagents(t *testing.T) {
	dir := testutil.Sessions(t)
	files := ListOmpFiles([]string{dir}, true, false)
	if len(files) != 2 {
		t.Fatalf("files: %v", files)
	}
	var sub ir.SourceFile
	for _, f := range files {
		if f.AgentName == "scout" {
			sub = f
		}
	}
	if sub.ParentKey != filepath.Join(dir, testutil.MainRel) {
		t.Fatalf("parent: %q", sub.ParentKey)
	}
	if n := len(ListOmpFiles([]string{dir}, false, false)); n != 1 {
		t.Fatalf("without subagents: %d", n)
	}
}

func TestRegistrySniffsAndOpens(t *testing.T) {
	dir := testutil.Sessions(t)
	f, err := FileFor(filepath.Join(dir, testutil.MainRel))
	if err != nil || f.Kind != ir.OMP {
		t.Fatalf("FileFor: %v %v", f.Kind, err)
	}
	s, err := Open(f)
	if err != nil || s.Cwd != testutil.CWD || s.SkipReason != "" || len(s.Lines(0)) == 0 {
		t.Fatalf("Open: %v %+v", err, s)
	}
	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.OMP: {dir}, ir.Claude: {dir}}}
	if n := len((OMP{}).Discover(cfg)); n != 2 {
		t.Fatalf("Discover: %d", n)
	}
	if n := len((Claude{}).Discover(cfg)); n != 0 {
		t.Fatalf("omp files must not be read as Claude: %d", n)
	}
}
