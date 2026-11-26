package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/veritas501/mem-grep/internal/ir"
)

// isolate points every variable the defaults read at a fresh directory.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, v := range []string{"PI_CODING_AGENT_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "QWEN_HOME", "XDG_DATA_HOME", "DSH_HOME", "ZCODE_STORAGE_DIR", "ZCODE_DATA_BASE_DIR"} {
		t.Setenv(v, "")
	}
	for _, k := range []ir.SourceKind{ir.OMP, ir.Claude, ir.Codex, ir.Gemini, ir.Qwen, ir.OpenCode, ir.Dsh, ir.ZCode, ir.Antigravity} {
		t.Setenv(EnvName(k), "")
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestDefaultDirs(t *testing.T) {
	home := isolate(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[ir.SourceKind][]string{
		ir.OMP:         {filepath.Join(home, ".omp", "agent", "sessions")},
		ir.Claude:      {filepath.Join(home, ".claude", "projects")},
		ir.Codex:       {filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions")},
		ir.Gemini:      {filepath.Join(home, ".gemini", "tmp")},
		ir.Qwen:        {filepath.Join(home, ".qwen", "projects")},
		ir.OpenCode:    {filepath.Join(home, ".local", "share", "opencode")},
		ir.Dsh:         {filepath.Join(home, ".dsh", "sessions")},
		ir.ZCode:       {filepath.Join(home, ".zcode", "cli")},
		ir.Antigravity: {filepath.Join(home, ".gemini", "antigravity-cli"), filepath.Join(home, ".gemini", "antigravity")},
	} {
		if got := c.Dirs(k); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", k, got, want)
		}
	}
	if !c.Subagents() || c.Advisor() {
		t.Errorf("defaults: subagents=%v advisor=%v", c.Subagents(), c.Advisor())
	}
}

func TestAgentsOwnVariablesMoveTheDefaults(t *testing.T) {
	isolate(t)
	t.Setenv("CODEX_HOME", "/x/codex")
	t.Setenv("GEMINI_CLI_HOME", "/x/gem")
	t.Setenv("QWEN_HOME", "/x/qwen")
	t.Setenv("XDG_DATA_HOME", "/x/data")
	t.Setenv("PI_CODING_AGENT_DIR", "/x/pi")
	t.Setenv("DSH_HOME", "/x/dsh")
	t.Setenv("ZCODE_STORAGE_DIR", " /x/zstore ")
	t.Setenv("ZCODE_DATA_BASE_DIR", "/x/zdata")
	c, _ := Load()
	for k, want := range map[ir.SourceKind][]string{
		ir.Codex:    {filepath.FromSlash("/x/codex/sessions"), filepath.FromSlash("/x/codex/archived_sessions")},
		ir.Gemini:   {filepath.FromSlash("/x/gem/.gemini/tmp")},
		ir.Qwen:     {filepath.FromSlash("/x/qwen/projects")},
		ir.OpenCode: {filepath.FromSlash("/x/data/opencode")},
		ir.Dsh:      {filepath.FromSlash("/x/dsh/sessions")},
		ir.ZCode:    {filepath.FromSlash("/x/zstore/cli"), filepath.FromSlash("/x/zdata/.zcode/cli")},
	} {
		if got := c.Dirs(k); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", k, got, want)
		}
	}
	if got := c.Dirs(ir.OMP); len(got) < 2 || got[0] != filepath.FromSlash("/x/pi/sessions") {
		t.Errorf("omp: %v", got)
	}
}

func TestEnvironmentReplacesTheDefaults(t *testing.T) {
	isolate(t)
	sep := string(os.PathListSeparator)
	t.Setenv("MG_CODEX_DIRS", "/a"+sep+sep+"/b c")
	t.Setenv("MG_OPENCODE_DIRS", "/db")
	c, _ := Load()
	// each directory is cleaned (on Windows that also turns / into \)
	if got := c.Dirs(ir.Codex); !slices.Equal(got, []string{filepath.FromSlash("/a"), filepath.FromSlash("/b c")}) {
		t.Errorf("codex: %v", got)
	}
	if got := c.Dirs(ir.OpenCode); !slices.Equal(got, []string{filepath.FromSlash("/db")}) {
		t.Errorf("opencode: %v", got)
	}
	if got := c.Dirs(ir.Claude); len(got) != 1 || filepath.Base(got[0]) != "projects" {
		t.Errorf("an agent without a variable keeps its defaults: %v", got)
	}
	// explicit directories (tests, callers) win over the environment
	c.AgentDirs = map[ir.SourceKind][]string{ir.Codex: {"/explicit"}}
	if got := c.Dirs(ir.Codex); !slices.Equal(got, []string{"/explicit"}) {
		t.Errorf("explicit: %v", got)
	}
	if EnvName(ir.OpenCode) != "MG_OPENCODE_DIRS" {
		t.Errorf("EnvName: %s", EnvName(ir.OpenCode))
	}
}

func TestSwitches(t *testing.T) {
	isolate(t)
	t.Setenv("MG_SUBAGENTS", "0")
	t.Setenv("MG_ADVISOR", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Subagents() || !c.Advisor() {
		t.Errorf("subagents=%v advisor=%v", c.Subagents(), c.Advisor())
	}
	t.Setenv("MG_SUBAGENTS", "sometimes")
	if _, err := Load(); err == nil {
		t.Error("an invalid boolean must be an error, not silently ignored")
	}
}

// Issue #3: PI_CODING_AGENT_DIR pointing at the default pi location, or repeated entries in
// MG_OMP_DIRS, must not list any root twice — a duplicate root makes every pi session file (and
// every matching turn) appear twice in searches.
func TestSessionDirsAreNotDuplicated(t *testing.T) {
	home := isolate(t)
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))
	c, _ := Load()
	dedup := func(dirs []string) {
		t.Helper()
		seen := map[string]bool{}
		for _, d := range dirs {
			if seen[d] {
				t.Errorf("duplicate root %s in %v", d, dirs)
			}
			seen[d] = true
		}
	}
	dedup(c.Dirs(ir.OMP))
	if got := c.Dirs(ir.OMP); len(got) != 2 {
		t.Errorf("omp roots: %v, want the pi and omp sessions only", got)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("MG_OMP_DIRS", "/a"+sep+filepath.Join(home, ".pi", "agent", "sessions")+sep+"/a"+sep+filepath.FromSlash("/a/../a"))
	c2, _ := Load()
	dedup(c2.Dirs(ir.OMP))
	if got := c2.Dirs(ir.OMP); len(got) != 2 {
		t.Errorf("omp roots from MG_OMP_DIRS: %v, want /a and the pi sessions only", got)
	}
}
