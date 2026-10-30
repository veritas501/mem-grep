// Package mg implements mg (mem-grep): rg-style search over past coding-agent sessions.
//
// Designed for AI agents first: every result carries a reference that the next command accepts
// (omp:01a180aa#3), output is compact and line-oriented (or JSON Lines with --json), a footer
// starting with "-- " summarizes and suggests the next step, and exit codes follow rg
// (0 found, 1 nothing found, 2 error). No index, no model, no network.
package mg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/grep"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/sources"
	"github.com/veritas501/mem-grep/internal/util"
	"github.com/veritas501/mem-grep/internal/version"
)

const long = `mg (mem-grep) searches past coding-agent sessions (omp, Claude Code, Codex) like rg searches
files: no index, no model, no network. Each result is one turn — a user message and everything
the agent did in response — with a reference to expand it.

Terms:
  Every TERM must occur in the same turn. Inside a term, | separates alternatives, so one call
  covers synonyms and both languages:   mg docker '网络|network'
  Matching is literal and case-insensitive; -e makes terms RE2 regexes, -s case-sensitive.

Typical flow:
  mg docker '网络|network' '修|fix'                  find the turns
  mg show omp:01a180aa#12 -A 3                        read that turn and the 3 after it
  mg -r user 'cli|命令行' '库|library'                 what the user asked for or decided
  mg -e 'github\.com/[\w.-]+/[\w.-]+' -r tool --top   which modules were used most
  mg sessions -p shop-api --since 2w                  what happened in a project lately

Output: a header per turn "<ref>  <time>  <project>  (turn i/n)", its first user message, then
matched lines marked ">" (cut around the match; --full for whole turns; -r also limits what is
printed). Lines use the transcript notation: "USER:", "ASSISTANT:", "→ tool(args)",
"✓ tool: result", "✗ tool failed: …".
The footer ("-- …") says how many turns matched and what to try next. Sessions are read newest
first and the search stops once older ones cannot make the -n newest; the count then reads
"at least" (-n 0 for exact totals). --json prints JSON Lines.
Exit status: 0 = matches, 1 = no matches, 2 = error, 3 = nothing found but --timeout cut the search
short (inconclusive). --max-chars N caps what is printed.`

type options struct {
	agents   []string
	project  string
	since    string
	until    string
	paths    []string
	asJSON   bool
	color    string
	regex    bool
	caseSens bool
	word     bool
	any      bool
	roles    []string
	limit    int
	context  int
	full     bool
	width    int
	top      bool
	after    int
	before   int
	timeout  time.Duration
	maxChars int
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// errNoMatch makes the command exit 1 without printing an error.
var errNoMatch = exitCode(1)

// errInconclusive (exit 3): nothing matched in what was read, but --timeout stopped the search
// before it was done, so the answer is unknown. Not 1, which means "searched everything, nothing".
var errInconclusive = exitCode(3)

// NewRoot builds the mg command tree.
func NewRoot() *cobra.Command {
	o := &options{}
	root := &cobra.Command{
		Use:           "mg [flags] TERM...",
		Short:         "Search past coding-agent sessions like rg (no index, no model, no network)",
		Long:          long,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return runSearch(cmd, o, args)
		},
	}
	root.SetVersionTemplate(version.String("mg") + "\n")
	root.CompletionOptions.DisableDefaultCmd = true
	pf := root.PersistentFlags()
	pf.StringSliceVarP(&o.agents, "agent", "a", nil, "only these agents: omp, claude, codex, gemini, qwen, opencode (repeatable or comma-separated)")
	pf.StringVarP(&o.project, "project", "p", "", "only sessions whose working directory contains this text")
	pf.StringVar(&o.since, "since", "", "only turns at or after this time: 36h, 7d, 2w, 3m, 1y or 2026-10-01")
	pf.StringVar(&o.until, "until", "", "only turns before this time (same forms as --since)")
	pf.StringArrayVar(&o.paths, "path", nil, "search these session files or directories instead of the agents' default locations")
	pf.BoolVar(&o.asJSON, "json", false, "print JSON Lines (one object per line, each with a \"type\")")
	pf.StringVar(&o.color, "color", "auto", "highlight matches: auto, always, never")
	pf.IntVarP(&o.limit, "limit", "n", 20, "show at most N results (0 = all)")
	pf.IntVar(&o.maxChars, "max-chars", 0, "stop printing once about N characters are out, with a note of what is left (0 = no limit)")
	pf.IntVar(&o.width, "width", 240, "cut long lines to about N characters around the match (0 = never)")

	addSearchFlags(root, o)
	search := &cobra.Command{
		Use:   "search TERM...",
		Short: "Search (the default command; use it when a term is also a command name)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runSearch(cmd, o, args) },
	}
	addSearchFlags(search, o)
	show := &cobra.Command{
		Use:   "show REF...",
		Short: "Print turns of a session: REF is agent:id#turn, agent:id#from-to, agent:id or a file path",
		Args:  cobra.MinimumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runShow(cmd, o, args) },
	}
	show.Flags().IntVarP(&o.after, "after", "A", 0, "also print N turns after the referenced ones")
	show.Flags().BoolVar(&o.full, "full", false, "print lines uncut (the default unless --width is given)")
	show.Flags().StringSliceVarP(&o.roles, "role", "r", nil, "only print lines of these roles (the opening line of each turn is always kept)")
	show.Flags().IntVarP(&o.before, "before", "B", 0, "also print N turns before the referenced ones")
	sessions := &cobra.Command{
		Use:   "sessions [TERM...]",
		Short: "List sessions, newest first; with TERMs, only sessions containing a matching turn",
		RunE:  func(cmd *cobra.Command, args []string) error { return runSessions(cmd, o, args) },
	}
	addSearchFlags(sessions, o)
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the build identity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String("mg"))
			return nil
		},
	}
	root.AddCommand(search, show, sessions, versionCmd)
	return root
}

func addSearchFlags(c *cobra.Command, o *options) {
	f := c.Flags()
	f.DurationVar(&o.timeout, "timeout", 0, "stop searching after this long (2s, 500ms; 0 = no limit); older sessions are then left unread and a miss proves nothing (exit 3)")
	f.BoolVarP(&o.regex, "regex", "e", false, "terms are RE2 regular expressions")
	f.BoolVarP(&o.caseSens, "case-sensitive", "s", false, "match case (default: ignore case)")
	f.BoolVarP(&o.word, "word", "w", false, "match whole words only")
	f.BoolVar(&o.any, "any", false, "a turn matches when any term matches (default: all terms)")
	f.StringSliceVarP(&o.roles, "role", "r", nil, "only match lines of these roles: user, assistant, tool, result, shell, task, note")
	if c.Name() != "sessions" {
		f.IntVarP(&o.context, "context", "C", 0, "also print N lines around each matched line of the turn")
		f.BoolVar(&o.full, "full", false, "print whole turns, uncut")
		f.BoolVar(&o.top, "top", false, "count the distinct matched strings instead (most frequent first)")
	}
}

// Execute runs mg and returns its exit status.
func Execute(args []string) int {
	root := NewRoot()
	root.SetArgs(args)
	err := root.Execute()
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	default:
		fmt.Fprintln(root.ErrOrStderr(), "mg: "+err.Error())
		return 2
	}
}

// ---------------------------------------------------------------- setup

func (o *options) query(terms []string) (grep.Query, error) {
	q := grep.Query{Patterns: terms, Regex: o.regex, CaseSensitive: o.caseSens, Word: o.word, Any: o.any, Project: o.project}
	now := time.Now()
	var err error
	if o.since != "" {
		if q.Since, err = grep.ParseTimeBound(o.since, now); err != nil {
			return q, err
		}
	}
	if o.until != "" {
		if q.Until, err = grep.ParseTimeBound(o.until, now); err != nil {
			return q, err
		}
	}
	if q.Roles, err = o.parseRoles(); err != nil {
		return q, err
	}
	agents, err := o.agentKinds()
	q.Agents = agents
	return q, err
}

func (o *options) parseRoles() ([]ir.Role, error) {
	var out []ir.Role
	for _, r := range o.roles {
		role := ir.Role(strings.ToLower(strings.TrimSpace(r)))
		switch role {
		case ir.RoleUser, ir.RoleAssistant, ir.RoleTool, ir.RoleResult, ir.RoleShell, ir.RoleTask, ir.RoleNote:
			out = append(out, role)
		default:
			return nil, fmt.Errorf("unknown role %q (user, assistant, tool, result, shell, task, note)", r)
		}
	}
	return out, nil
}

// roleSet is the -r filter for printing (nil: every role); call it after parseRoles validated it.
func (o *options) roleSet() map[ir.Role]bool {
	roles, _ := o.parseRoles()
	if len(roles) == 0 {
		return nil
	}
	set := map[ir.Role]bool{}
	for _, r := range roles {
		set[r] = true
	}
	return set
}

func kindNames() string {
	var names []string
	for _, k := range sources.Kinds() {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}

func (o *options) agentKinds() ([]ir.SourceKind, error) {
	var out []ir.SourceKind
	for _, a := range o.agents {
		k := ir.SourceKind(strings.ToLower(strings.TrimSpace(a)))
		if sources.Get(k) == nil {
			return nil, fmt.Errorf("unknown agent %q (%s)", a, kindNames())
		}
		out = append(out, k)
	}
	return out, nil
}

// files lists the sessions to search: --path, or every supported agent's session roots (the
// defaults, or MG_<AGENT>_DIRS).
func (o *options) files() ([]ir.SourceFile, error) {
	if len(o.paths) > 0 {
		var out []ir.SourceFile
		for _, p := range o.paths {
			if a, err := filepath.Abs(p); err == nil {
				p = a // results print paths; keep them usable from any directory
			}
			fs, err := sources.FilesUnder(p)
			if err != nil {
				return nil, err
			}
			out = append(out, fs...)
		}
		return out, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return sources.Discover(cfg), nil
}

type printer struct {
	w     io.Writer
	color bool
	json  bool
}

func newPrinter(cmd *cobra.Command, o *options) (*printer, error) {
	p := &printer{w: cmd.OutOrStdout(), json: o.asJSON}
	switch o.color {
	case "always":
		p.color = true
	case "never":
	case "auto":
		if f, ok := p.w.(*os.File); ok && !o.asJSON {
			if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
				p.color = true
			}
		}
	default:
		return nil, fmt.Errorf("--color must be auto, always or never")
	}
	return p, nil
}

func (p *printer) emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(p.w, string(b))
}

func (p *printer) highlight(text string, ranges [][2]int) string {
	if !p.color || len(ranges) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, r := range mergeRanges(ranges) {
		if r[0] < last {
			continue
		}
		b.WriteString(text[last:r[0]])
		b.WriteString("\x1b[1;31m" + text[r[0]:r[1]] + "\x1b[0m")
		last = r[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func mergeRanges(rs [][2]int) [][2]int {
	out := append([][2]int{}, rs...)
	for i := 1; i < len(out); i++ { // small n: insertion sort by start
		for j := i; j > 0 && out[j][0] < out[j-1][0]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	var merged [][2]int
	for _, r := range out {
		if n := len(merged); n > 0 && r[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func short(ts string) string {
	if t, ok := util.ParseTime(ts); ok {
		return t.Local().Format("2006-01-02 15:04")
	}
	if len(ts) > 16 {
		return strings.Replace(ts[:16], "T", " ", 1)
	}
	return ts
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	if strings.HasSuffix(word, "ch") || strings.HasSuffix(word, "s") {
		return fmt.Sprintf("%d %ses", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
