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
	for _, v := range []string{"PI_CODING_AGENT_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "QWEN_HOME", "XDG_DATA_HOME"} {
		t.Setenv(v, "")
	}
	for _, k := range []ir.SourceKind{ir.OMP, ir.Claude, ir.Codex, ir.Gemini, ir.Qwen, ir.OpenCode} {
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
		ir.OMP:      {filepath.Join(home, ".omp", "agent", "sessions")},
		ir.Claude:   {filepath.Join(home, ".claude", "projects")},
		ir.Codex:    {filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions")},
		ir.Gemini:   {filepath.Join(home, ".gemini", "tmp")},
		ir.Qwen:     {filepath.Join(home, ".qwen", "projects")},
		ir.OpenCode: {filepath.Join(home, ".local", "share", "opencode")},
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
	c, _ := Load()
	for k, want := range map[ir.SourceKind][]string{
		ir.Codex:    {filepath.FromSlash("/x/codex/sessions"), filepath.FromSlash("/x/codex/archived_sessions")},
		ir.Gemini:   {filepath.FromSlash("/x/gem/.gemini/tmp")},
		ir.Qwen:     {filepath.FromSlash("/x/qwen/projects")},
		ir.OpenCode: {filepath.FromSlash("/x/data/opencode")},
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
	if got := c.Dirs(ir.Codex); !slices.Equal(got, []string{"/a", "/b c"}) {
		t.Errorf("codex: %v", got)
	}
	if got := c.Dirs(ir.OpenCode); !slices.Equal(got, []string{"/db"}) {
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
