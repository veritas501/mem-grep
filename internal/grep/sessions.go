package grep

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/sources"
)

// Ref points at a session and optionally a range of its turns.
type Ref struct {
	Agent  ir.SourceKind // "" = any
	ID     string        // session id prefix, or a file path
	IsPath bool
	// From and To are 1-based turn numbers; 0 means "not given".
	From, To int
}

var refRe = func() *regexp.Regexp {
	var names []string
	for _, k := range sources.Kinds() {
		names = append(names, regexp.QuoteMeta(string(k)))
	}
	return regexp.MustCompile(`^(?:(` + strings.Join(names, "|") + `):)?([^#]+?)(?:#(\d+)(?:-(\d+))?)?$`)
}()

// ParseRef reads "omp:01a180aa#3", "01a180aa#3-5", "01a180aa" or a file path (optionally #N).
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	m := refRe.FindStringSubmatch(s)
	if m == nil {
		return Ref{}, fmt.Errorf("cannot read session reference %q (use agent:id#turn, e.g. omp:01a180aa#3)", s)
	}
	r := Ref{Agent: ir.SourceKind(m[1]), ID: m[2]}
	if m[3] != "" {
		r.From, _ = strconv.Atoi(m[3])
		r.To = r.From
	}
	if m[4] != "" {
		r.To, _ = strconv.Atoi(m[4])
	}
	if r.To < r.From {
		r.From, r.To = r.To, r.From
	}
	if strings.ContainsRune(r.ID, filepath.Separator) {
		if _, err := os.Stat(r.ID); err == nil {
			r.IsPath = true
		}
	}
	return r, nil
}

// Resolve finds the one session a reference names. File names are checked first (omp, Codex and
// Claude Code all put the session id in the file name); otherwise sessions are opened and their
// ids compared.
func Resolve(files []ir.SourceFile, r Ref) (*ir.Session, error) {
	if r.IsPath {
		fs, err := sources.FilesUnder(r.ID)
		if err != nil {
			return nil, err
		}
		if len(fs) != 1 {
			return nil, fmt.Errorf("%s: not a single session file", r.ID)
		}
		return openOne(fs[0])
	}
	var byName []ir.SourceFile
	for _, f := range files {
		if (r.Agent == "" || f.Kind == r.Agent) && (strings.Contains(filepath.Base(f.Path), r.ID) || (f.Inner != "" && strings.HasPrefix(f.Inner, r.ID))) {
			byName = append(byName, f)
		}
	}
	candidates := byName
	if len(candidates) == 0 {
		for _, f := range files {
			if r.Agent == "" || f.Kind == r.Agent {
				candidates = append(candidates, f)
			}
		}
	}
	var found []*ir.Session
	for _, f := range candidates {
		s, err := sources.Open(f)
		if err != nil || s == nil {
			continue
		}
		if strings.HasPrefix(s.SessionID, r.ID) || len(byName) > 0 {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no session matches %q", r.ID)
	case 1:
		return found[0], nil
	}
	var names []string
	for _, s := range found {
		names = append(names, Info(s, 0).Ref()+" "+s.File.Key)
	}
	return nil, fmt.Errorf("%q is ambiguous:\n  %s", r.ID, strings.Join(names, "\n  "))
}

func openOne(f ir.SourceFile) (*ir.Session, error) {
	s, err := sources.Open(f)
	if err == nil && s == nil {
		err = fmt.Errorf("%s: not a session file", f.Path)
	}
	return s, err
}

// Listing is one row of a session list.
type Listing struct {
	SessionInfo
	// First is the first user message (or subagent task), for recognising the session.
	First string `json:"first"`
	// LastTS is the time of the last turn.
	LastTS string `json:"last"`
}

// List parses sessions in parallel and returns those matching the query's agent, project and
// time filters (patterns are ignored), newest activity first.
func List(files []ir.SourceFile, q Query) ([]Listing, error) {
	m := &Matcher{q: q, project: strings.ToLower(q.Project)}
	if len(q.Agents) > 0 {
		m.agents = map[ir.SourceKind]bool{}
		for _, a := range q.Agents {
			m.agents[a] = true
		}
	}
	var (
		mu  sync.Mutex
		out []Listing
		wg  sync.WaitGroup
	)
	// A listing needs the turns' structure and openings only: every other record is skimmed.
	skipAll := sources.ScanOptions{Keep: func([]byte, []ir.Role) bool { return false }}
	jobs := make(chan ir.SourceFile)
	for w := 0; w < workers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for f := range jobs {
				if m.agents != nil && !m.agents[f.Kind] {
					continue
				}
				raw, err := sources.ReadFileInto(f, buf)
				if err != nil {
					continue
				}
				if cap(raw) <= 64<<20 {
					buf = raw[:0]
				}
				s, err := sources.OpenBytes(f, raw, skipAll)
				if err != nil || s == nil || s.SkipReason != "" {
					continue
				}
				if m.project != "" && !strings.Contains(strings.ToLower(s.Cwd), m.project) {
					continue
				}
				turns := SplitTurns(s.Lines(0))
				if len(turns) == 0 {
					continue
				}
				last := turns[len(turns)-1].TS
				if !m.q.Since.IsZero() && !m.inWindow(last) && !m.inWindow(turns[0].TS) {
					continue
				}
				first := ""
				for _, l := range turns[0].Lines {
					if l.Role == ir.RoleUser || l.Role == ir.RoleTask {
						first = l.Text
						break
					}
				}
				mu.Lock()
				out = append(out, Listing{SessionInfo: Info(s, len(turns)), First: first, LastTS: last})
				mu.Unlock()
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	sortListings(out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func sortListings(ls []Listing) {
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].LastTS != ls[j].LastTS {
			return ls[i].LastTS > ls[j].LastTS
		}
		return ls[i].Key < ls[j].Key
	})
}
