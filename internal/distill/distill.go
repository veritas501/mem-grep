// Package distill is the state behind mgd: which turns of which sessions have been read for
// memories (the ledger), and the memories themselves (a review queue and the store mg searches).
//
// It holds no model. Distilling — reading the unprocessed turns and deciding what is worth
// keeping — is done by an agent that calls mgd (skill/mem-distill); this package only finds the
// work, validates what the agent hands back and keeps the files.
//
//	$MG_HOME/ledger.json     session key -> turns already processed
//	$MG_HOME/queue/<id>.md   memories waiting for review
//	$MG_HOME/memories/<id>.md  accepted memories (read by mg's `mem` adapter)
package distill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/veritas501/mem-grep/internal/grep"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/sources"
	"github.com/veritas501/mem-grep/internal/util"
)

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

// Mark records how far a session has been processed.
type Mark struct {
	// Turns is the number of turns already read for memories.
	Turns int `json:"turns"`
	// MtimeMs is the session's modification time when it was marked: an unchanged session is
	// skipped without being opened.
	MtimeMs float64 `json:"mtime_ms"`
	At      string  `json:"at"`
}

// Ledger maps session keys to marks.
type Ledger struct {
	path     string
	Sessions map[string]Mark `json:"sessions"`
}

// LoadLedger reads the ledger of a mem-grep home; a missing file is an empty ledger.
func LoadLedger(home string) (*Ledger, error) {
	l := &Ledger{path: filepath.Join(home, "ledger.json"), Sessions: map[string]Mark{}}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("%s: %w", l.path, err)
	}
	if l.Sessions == nil {
		l.Sessions = map[string]Mark{}
	}
	return l, nil
}

// Save writes the ledger atomically.
func (l *Ledger) Save() error {
	data, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	return writeAtomic(l.path, append(data, '\n'))
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Pending work
// ---------------------------------------------------------------------------

// Signals a turn can carry: cheap, deterministic hints that it may hold something worth keeping.
const (
	SignalCorrection = "correction" // the user corrected the agent or stated a rule
	SignalRecovered  = "recovered"  // a tool call failed and a later one succeeded
	SignalRewound    = "rewound"    // the user went back and abandoned a path
	SignalAborted    = "aborted"    // the user interrupted the agent
)

var correctionRe = regexp.MustCompile(`(?i)不要|别再|别用|不对|不是这|错了|应该|以后|一律|必须|记住|always|never|don'?t|do not|instead|wrong|prefer|remember|from now on|stop `)

// TurnSignals returns the signals of one turn, in a fixed order.
func TurnSignals(t grep.Turn) []string {
	seen := map[string]bool{}
	failed := false
	for _, l := range t.Lines {
		switch {
		case l.Role == ir.RoleUser && correctionRe.MatchString(l.Text):
			seen[SignalCorrection] = true
		case l.Role == ir.RoleResult && l.Success != nil && !*l.Success:
			failed = true
		case l.Role == ir.RoleResult && l.Success != nil && *l.Success && failed:
			seen[SignalRecovered] = true
		case l.Role == ir.RoleNote && strings.HasPrefix(l.Text, "[branch:"):
			seen[SignalRewound] = true
		case l.Role == ir.RoleNote && strings.Contains(l.Text, "aborted by the user"):
			seen[SignalAborted] = true
		}
	}
	var out []string
	for _, s := range []string{SignalCorrection, SignalRecovered, SignalRewound, SignalAborted} {
		if seen[s] {
			out = append(out, s)
		}
	}
	return out
}

// Pending is a session with turns not yet processed.
type Pending struct {
	Info grep.SessionInfo
	// Turns are all turns of the session; the unprocessed ones start at index Done.
	Turns   []grep.Turn
	Done    int
	Signals map[string]int // over the unprocessed turns
	LastTS  string
	MtimeMs float64
}

// Options select the sessions FindPending looks at.
type Options struct {
	Agents  []ir.SourceKind
	Project string
	// Idle leaves out sessions written less than this long ago: they may still be going on.
	Idle time.Duration
	Now  time.Time
	// Since and Until bound the session's last activity; zero means unbounded.
	Since, Until time.Time
}

// conversation reports whether a file can hold a conversation worth distilling: not a subagent's
// session (its lesson is in the parent's), not a memory.
func conversation(f ir.SourceFile) bool {
	return f.ParentKey == "" && !f.IsAdvisor && f.Kind != ir.Mem
}

// FindPending opens the sessions that changed since they were marked and returns those with
// unprocessed turns, most recent first.
func FindPending(files []ir.SourceFile, l *Ledger, opt Options) []Pending {
	agents := map[ir.SourceKind]bool{}
	for _, a := range opt.Agents {
		agents[a] = true
	}
	project := strings.ToLower(opt.Project)
	// Cheap checks first, on what the listing already knows; the sessions left are opened in
	// parallel (a first run over a long history opens every one of them).
	var todo []ir.SourceFile
	for _, f := range files {
		if !conversation(f) || (len(agents) > 0 && !agents[f.Kind]) {
			continue
		}
		if mark, marked := l.Sessions[f.Key]; marked && f.MtimeMs <= mark.MtimeMs {
			continue
		}
		if opt.Idle > 0 && float64(opt.Now.UnixNano())/1e6-f.MtimeMs < float64(opt.Idle.Milliseconds()) {
			continue
		}
		// a session last written before Since has no activity after it
		if !opt.Since.IsZero() && f.MtimeMs < float64(opt.Since.UnixNano())/1e6 {
			continue
		}
		todo = append(todo, f)
	}
	results := make([]*Pending, len(todo))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				p, ok := open(todo[i], l.Sessions[todo[i].Key].Turns)
				if !ok || len(p.Turns) <= p.Done {
					continue
				}
				if project != "" && !strings.Contains(strings.ToLower(p.Info.Cwd), project) {
					continue
				}
				if last, ok := util.ParseTime(p.LastTS); ok && ((!opt.Since.IsZero() && last.Before(opt.Since)) || (!opt.Until.IsZero() && !last.Before(opt.Until))) {
					continue
				}
				results[i] = &p
			}
		}()
	}
	for i := range todo {
		next <- i
	}
	close(next)
	wg.Wait()
	var out []Pending
	for _, p := range results {
		if p != nil {
			out = append(out, *p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out
}

// open parses a session into a Pending; ok is false for what is not a conversation.
func open(f ir.SourceFile, done int) (Pending, bool) {
	s, err := sources.Open(f)
	if err != nil || s == nil || s.SkipReason != "" {
		return Pending{}, false
	}
	lines := s.Lines(0)
	if len(lines) == 0 || lines[0].Role == ir.RoleMemory {
		return Pending{}, false
	}
	turns := grep.SplitTurns(lines)
	if done > len(turns) {
		done = len(turns)
	}
	p := Pending{Info: grep.Info(s, len(turns)), Turns: turns, Done: done, Signals: map[string]int{}, MtimeMs: f.MtimeMs}
	for _, t := range turns[done:] {
		for _, sig := range TurnSignals(t) {
			p.Signals[sig]++
		}
		if t.TS > p.LastTS {
			p.LastTS = t.TS
		}
	}
	return p, true
}

// OpenRef resolves a reference to a session and its progress.
func OpenRef(files []ir.SourceFile, l *Ledger, ref string) (Pending, grep.Ref, error) {
	r, err := grep.ParseRef(ref)
	if err != nil {
		return Pending{}, r, err
	}
	s, err := grep.Resolve(files, r)
	if err != nil {
		return Pending{}, r, err
	}
	p, ok := open(s.File, l.Sessions[s.File.Key].Turns)
	if !ok {
		return Pending{}, r, fmt.Errorf("%s: not a conversation", ref)
	}
	if p.MtimeMs == 0 {
		if st, err := os.Stat(s.File.Path); err == nil {
			p.MtimeMs = float64(st.ModTime().UnixNano()) / 1e6
		}
	}
	return p, r, nil
}

// MarkDone records that the first n turns of a session are processed.
func (l *Ledger) MarkDone(p Pending, n int, now time.Time) {
	if n > len(p.Turns) {
		n = len(p.Turns)
	}
	m := Mark{Turns: n, At: now.UTC().Format(time.RFC3339)}
	// Only a fully processed session may be skipped unopened while it does not change.
	if n == len(p.Turns) {
		m.MtimeMs = p.MtimeMs
	}
	l.Sessions[p.Info.Key] = m
}

// ---------------------------------------------------------------------------
// Memories: the review queue and the store
// ---------------------------------------------------------------------------

// Memory is one distilled memory.
type Memory struct {
	ID          string
	Name        string
	Description string
	Type        string
	Project     string
	Evidence    []string
	Created     string
	Body        string
}

// Types are the kinds of memory, as in Claude Code and ZCode.
var Types = []string{"user", "feedback", "project", "reference"}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,59}$`)

// MemoryID derives the file name of a memory: the same name in the same project is the same
// memory, so writing it again replaces it.
func MemoryID(project, name string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + name))
	return hex.EncodeToString(sum[:])[:8] + "-" + name
}

// Validate checks a memory an agent proposed and normalises it: text is redacted, the id is set.
// The evidence must already have been resolved by the caller.
func (m *Memory) Validate() error {
	m.Name = strings.TrimSpace(m.Name)
	m.Description = strings.Join(strings.Fields(util.Redact(m.Description)), " ")
	m.Body = strings.TrimSpace(util.Redact(m.Body))
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("name %q: use lower-case words joined by hyphens (a-z, 0-9, -), 2 to 60 characters", m.Name)
	}
	ok := false
	for _, t := range Types {
		ok = ok || t == m.Type
	}
	if !ok {
		return fmt.Errorf("type %q: use one of %s", m.Type, strings.Join(Types, ", "))
	}
	if n := len([]rune(m.Description)); n < 10 || n > 240 {
		return fmt.Errorf("description: one line of 10 to 240 characters (got %d)", n)
	}
	if n := len([]rune(m.Body)); n < 20 || n > 4000 {
		return fmt.Errorf("body: 20 to 4000 characters (got %d)", n)
	}
	if len(m.Evidence) == 0 {
		return errors.New("evidence: name at least one turn the memory comes from (agent:id#turn)")
	}
	m.ID = MemoryID(m.Project, m.Name)
	return nil
}

// Format renders a memory file. The description is quoted, with no character that needs escaping.
func (m Memory) Format() string {
	quote := func(s string) string {
		return `"` + strings.NewReplacer(`"`, `'`, `\`, `/`).Replace(s) + `"`
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\ntype: %s\nproject: %s\nevidence: [%s]\ncreated: %s\n---\n\n%s\n",
		m.Name, quote(m.Description), m.Type, `"`+m.Project+`"`, strings.Join(m.Evidence, ", "), m.Created, m.Body)
	return b.String()
}

// Store is a mem-grep home.
type Store struct{ Home string }

func (s Store) QueueDir() string  { return filepath.Join(s.Home, "queue") }
func (s Store) MemoryDir() string { return filepath.Join(s.Home, "memories") }

// Enqueue writes a validated memory into the review queue and returns its path.
func (s Store) Enqueue(m Memory) (string, error) {
	path := filepath.Join(s.QueueDir(), m.ID+".md")
	return path, writeAtomic(path, []byte(m.Format()))
}

// List reads the memories of a directory, oldest first.
func List(dir string) ([]Memory, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Memory
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		f := sources.ParseMemoryFile(string(data))
		out = append(out, Memory{ID: strings.TrimSuffix(e.Name(), ".md"), Name: f.Name, Description: f.Description, Type: f.Type,
			Project: f.Project, Evidence: f.Evidence, Created: f.Created, Body: f.Body})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out, nil
}

// Pick returns the memories of a directory whose id starts with one of the given prefixes; an
// unknown or ambiguous prefix is an error.
func Pick(dir string, prefixes []string) ([]Memory, error) {
	all, err := List(dir)
	if err != nil {
		return nil, err
	}
	var out []Memory
	for _, p := range prefixes {
		var found []Memory
		for _, m := range all {
			if strings.HasPrefix(m.ID, p) {
				found = append(found, m)
			}
		}
		switch len(found) {
		case 0:
			return nil, fmt.Errorf("no memory %q in %s", p, dir)
		case 1:
			out = append(out, found[0])
		default:
			return nil, fmt.Errorf("%q is ambiguous (%d memories)", p, len(found))
		}
	}
	return out, nil
}

// Accept moves a queued memory into the store, replacing an older memory of the same id.
func (s Store) Accept(id string) error {
	if err := os.MkdirAll(s.MemoryDir(), 0o755); err != nil {
		return err
	}
	return os.Rename(filepath.Join(s.QueueDir(), id+".md"), filepath.Join(s.MemoryDir(), id+".md"))
}

// Remove deletes a memory from a directory.
func Remove(dir, id string) error { return os.Remove(filepath.Join(dir, id+".md")) }
