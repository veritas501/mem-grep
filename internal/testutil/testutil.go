// Package testutil provides fixtures and a mock LLM for tests. LLMs are always mocked.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// CWD is the project directory recorded in the omp fixtures.
const CWD = "/home/dev/projects/shop-api"

// MainRel is the main fixture session, relative to the sessions directory.
const MainRel = "-projects-shop-api/main.jsonl"

// ScoutRel is the subagent fixture session.
const ScoutRel = "-projects-shop-api/main/scout.jsonl"

// FixtureDir is test/fixtures/sessions in the repository (sessions written by omp's own
// SessionManager, see test/fixtures/gen-fixtures.ts).
func FixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "test", "fixtures", "sessions")
}

// Fixture returns a path under test/fixtures (sessions the agents themselves wrote: gemini/tmp,
// qwen/projects, opencode/opencode.db; each directory has the gen.sh that makes them).
func Fixture(rel string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "test", "fixtures", filepath.FromSlash(rel))
}

// Sessions copies the fixtures into a fresh temporary directory, marks them idle for an hour
// (so CompleteBytes equals Size) and returns the copy's path.
func Sessions(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "sessions")
	if err := os.CopyFS(dst, os.DirFS(FixtureDir())); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, rel := range []string{MainRel, ScoutRel} {
		if err := os.Chtimes(filepath.Join(dst, rel), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}
