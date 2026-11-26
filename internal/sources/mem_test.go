package sources

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
)

const memFixture = `---
name: prefer-pnpm
description: "use pnpm in this repository, never npm"
type: feedback
project: "/home/dev/shop"
evidence: [omp:01a180aa-27cc#1, zcode:78b4656a-f8dd#4-5]
created: 2026-11-20T08:00:00Z
---

Use pnpm for every script.

**Why:** the lockfile is pnpm's; the token is sk-abcdefghijklmnopqrstuvwxyz012345.
`

func TestMemStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memories")
	path := filepath.Join(dir, "3fa91c2e-prefer-pnpm.md")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{path: memFixture, filepath.Join(dir, "sub", "nested.md"): memFixture, filepath.Join(dir, "README.md"): "# notes\n"} {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := (Mem{}).Discover(&config.Config{AgentDirs: map[ir.SourceKind][]string{ir.Mem: {dir}}})
	if len(files) != 2 { // the store is flat; README.md is listed and renders as a memory without a type
		t.Fatalf("%+v", files)
	}
	// given by path, only files with the store's front matter are memories
	under, err := FilesUnder(filepath.Dir(dir))
	if err != nil || len(under) != 1 || under[0].Kind != ir.Mem || under[0].Inner != "3fa91c2e-prefer-pnpm" || under[0].Key != path {
		t.Fatalf("%+v %v", under, err)
	}
	s, err := Open(under[0])
	if err != nil || s == nil || s.SessionID != "3fa91c2e-prefer-pnpm" || s.Cwd != "/home/dev/shop" || s.StartedAt != "2026-11-20T08:00:00.000Z" {
		t.Fatalf("%v %+v", err, s)
	}
	want := `MEMORY 3fa91c2e-prefer-pnpm.md [feedback]: use pnpm in this repository, never npm
Use pnpm for every script.
**Why:** the lockfile is pnpm's; the token is [REDACTED].
[evidence: omp:01a180aa-27cc#1 zcode:78b4656a-f8dd#4-5]`
	lines := s.Lines(0)
	if got := ir.Text(lines); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	for i, l := range lines {
		if l.Role != ir.RoleMemory || l.TurnStart != (i == 0) {
			t.Fatalf("line %d: %+v", i, l)
		}
	}
	raw, err := ReadFileInto(under[0], nil)
	if err != nil || int64(len(raw)) != s.CompleteBytes {
		t.Fatalf("stream %d bytes vs %d (%v)", len(raw), s.CompleteBytes, err)
	}
}
