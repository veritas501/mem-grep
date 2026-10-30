// Package config resolves where each agent keeps its sessions. mg has no configuration file:
// everything is set through environment variables.
//
//	MG_<AGENT>_DIRS  session roots of one agent, separated like PATH (":" on Unix): MG_OMP_DIRS,
//	                 MG_CLAUDE_DIRS, MG_CODEX_DIRS, MG_GEMINI_DIRS, MG_QWEN_DIRS, MG_OPENCODE_DIRS.
//	                 Replaces that agent's default roots.
//	MG_SUBAGENTS     search subagent sessions too (default 1)
//	MG_ADVISOR       search omp advisor runs too (default 0)
//
// The defaults follow each agent's own conventions, including the variables the agents
// themselves honour (PI_CODING_AGENT_DIR, CODEX_HOME, GEMINI_CLI_HOME, QWEN_HOME, XDG_DATA_HOME).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/veritas501/mem-grep/internal/ir"
)

// Config is the resolved configuration.
type Config struct {
	// AgentDirs overrides the session roots of an agent; set by tests. Dirs falls back to the
	// environment and then to the defaults.
	AgentDirs        map[ir.SourceKind][]string
	IncludeSubagents *bool
	IncludeAdvisor   *bool
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func defaultSessionDirs() []string {
	var dirs []string
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		dirs = append(dirs, filepath.Join(d, "sessions"))
	}
	omp := filepath.Join(homeDir(), ".omp", "agent", "sessions")
	if len(dirs) == 0 || dirs[0] != omp {
		dirs = append(dirs, omp)
	}
	if pi := filepath.Join(homeDir(), ".pi", "agent", "sessions"); exists(pi) {
		dirs = append(dirs, pi)
	}
	return dirs
}

func defaultCodexDirs() []string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = filepath.Join(homeDir(), ".codex")
	}
	return []string{filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")}
}

// envBool reads a boolean variable; unset or empty means def.
func envBool(name string, def bool) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q is not a boolean (use 1 or 0)", name, v)
	}
	return b, nil
}

// Load reads the settings from the environment.
func Load() (*Config, error) {
	sub, err := envBool("MG_SUBAGENTS", true)
	if err != nil {
		return nil, err
	}
	adv, err := envBool("MG_ADVISOR", false)
	if err != nil {
		return nil, err
	}
	return &Config{IncludeSubagents: &sub, IncludeAdvisor: &adv}, nil
}

// EnvName is the variable that sets an agent's session roots, e.g. MG_CODEX_DIRS.
func EnvName(k ir.SourceKind) string { return "MG_" + strings.ToUpper(string(k)) + "_DIRS" }

// Dirs returns the session roots of an agent: AgentDirs, else MG_<AGENT>_DIRS, else the defaults.
func (c *Config) Dirs(k ir.SourceKind) []string {
	if d := c.AgentDirs[k]; len(d) > 0 {
		return d
	}
	if v := os.Getenv(EnvName(k)); v != "" {
		var dirs []string
		for _, d := range filepath.SplitList(v) {
			if d != "" {
				dirs = append(dirs, d)
			}
		}
		if len(dirs) > 0 {
			return dirs
		}
	}
	return DefaultDirs(k)
}

// DefaultDirs are the places an agent keeps its sessions, from the agent's own source:
//   - omp:    ~/.omp/agent/sessions, and ~/.pi/agent/sessions if present (PI_CODING_AGENT_DIR is honoured)
//   - claude: ~/.claude/projects
//   - codex:  $CODEX_HOME/sessions and $CODEX_HOME/archived_sessions (CODEX_HOME defaults to ~/.codex)
//   - gemini: ~/.gemini/tmp (GEMINI_CLI_HOME replaces ~); sessions are <tmp>/<project>/chats/*.jsonl
//   - qwen:   ~/.qwen/projects (QWEN_HOME replaces ~/.qwen); sessions are <project>/chats/*.jsonl
//   - opencode: $XDG_DATA_HOME/opencode (~/.local/share/opencode); one SQLite database, opencode.db
func DefaultDirs(k ir.SourceKind) []string {
	switch k {
	case ir.OMP:
		return defaultSessionDirs()
	case ir.Claude:
		return []string{filepath.Join(homeDir(), ".claude", "projects")}
	case ir.Codex:
		return defaultCodexDirs()
	case ir.Gemini:
		home := os.Getenv("GEMINI_CLI_HOME")
		if home == "" {
			home = homeDir()
		}
		return []string{filepath.Join(home, ".gemini", "tmp")}
	case ir.OpenCode:
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(homeDir(), ".local", "share")
		}
		return []string{filepath.Join(data, "opencode")}
	case ir.Qwen:
		dir := os.Getenv("QWEN_HOME")
		if dir == "" {
			dir = filepath.Join(homeDir(), ".qwen")
		}
		return []string{filepath.Join(dir, "projects")}
	}
	return nil
}

// Subagents reports whether subagent sessions are read.
func (c *Config) Subagents() bool { return c.IncludeSubagents == nil || *c.IncludeSubagents }

// Advisor reports whether omp advisor runs are read.
func (c *Config) Advisor() bool { return c.IncludeAdvisor != nil && *c.IncludeAdvisor }
