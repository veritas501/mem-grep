// Package grep searches past agent sessions the way rg searches files: no index, no model, no
// network. Every query scans the session logs found by the source adapters, renders them to the
// IR and matches the rendered transcript turn by turn.
//
// The unit of a result is a turn (one user message plus everything the agent did in response),
// because that is what answers "how did we fix it": several search terms must all occur within
// the same turn (unless Any is set), and a hit can be expanded to its neighbouring turns.
package grep

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/sources"
	"github.com/veritas501/mem-grep/internal/util"
)

// Query describes a search.
type Query struct {
	// Patterns are the search terms. Every term must match within the same turn (unless Any).
	// A literal term may list alternatives separated by "|" ("网络|network"); with Regex set,
	// terms are RE2 regular expressions.
	Patterns []string
	Regex    bool
	// CaseSensitive turns off the default case-insensitive matching. (No smart case: agents
	// capitalise words casually, and one capital letter silently hiding matches is a trap.)
	CaseSensitive bool
	Word          bool // match whole words (ASCII word boundaries)
	// Any makes a turn match when one pattern matches; by default every pattern must match
	// somewhere in the same turn.
	Any bool
	// Roles restricts matching to lines of these roles (empty: all).
	Roles []ir.Role
	// Agents restricts the search to these agents (empty: all).
	Agents []ir.SourceKind
	// Project keeps sessions whose cwd contains this substring (case-insensitive).
	Project string
	// Since / Until bound a turn's timestamp; zero means unbounded.
	Since, Until time.Time
	// Limit caps the number of turns returned (newest first); 0 = no limit.
	Limit int
	// Timeout bounds the time spent searching (0 = none). Files not yet read when it expires are
	// left unread, newest first: the result is then incomplete (Stats.TimedOut), and finding
	// nothing proves nothing.
	Timeout time.Duration
	// Partial lets hits carry only the lines the search decoded (the others Elided), for callers
	// that use matched lines only (--top, counts); otherwise their sessions are read again.
	Partial bool
}

// SessionInfo identifies a session in results.
type SessionInfo struct {
	Agent ir.SourceKind `json:"agent"`
	ID    string        `json:"session"`
	Path  string        `json:"path"`
	// Key identifies the session among all (Path is shared by the sessions of a database).
	Key       string `json:"-"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title,omitempty"`
	StartedAt string `json:"started"`
	Subagent  string `json:"subagent,omitempty"`
	Turns     int    `json:"turns"`
}

// Ref is the short handle printed with results and accepted by Resolve: "<agent>:<id prefix>".
// The prefix is 13 characters: omp and Codex ids are UUIDv7, whose first 8 hex digits change
// only every ~65 seconds, while 13 ("01a180aa-27cc") cover the full millisecond timestamp.
func (s SessionInfo) Ref() string {
	id := s.ID
	if len(id) > 13 {
		id = id[:13]
	}
	return string(s.Agent) + ":" + id
}

// Turn is one turn of a session with its 1-based index.
type Turn struct {
	Index int       `json:"turn"`
	TS    string    `json:"ts"`
	Lines []ir.Line `json:"-"`
}

// Hit is a matching turn.
type Hit struct {
	Session SessionInfo
	Turn    Turn
	// Matched holds, for each line of the turn, the byte ranges of the matches ([start, end]
	// pairs); lines without matches have none.
	Matched [][][2]int

	file ir.SourceFile
	// partial: Turn.Lines holds only the lines the search decoded (others are Elided).
	partial bool
}

// Stats describes the work a search did.
type Stats struct {
	Files, Skipped, Parsed, Sessions, Turns int
	// Matched counts the matching turns found. When Complete is false the search stopped early,
	// because older sessions could no longer make it into the Limit newest results, and Matched
	// is a lower bound.
	Matched, MatchedSessions int
	Complete                 bool
	// Unread counts the files left unread by an early stop or the timeout; TimedOut reports the
	// timeout (Complete is false then too).
	Unread   int
	TimedOut bool
	Elapsed  time.Duration
}

// term is one compiled search term.
type term struct {
	re *regexp.Regexp
	// alts are the literal alternatives (lower-cased when folding) matched without the regexp
	// engine; nil for regexes and for terms whose case folding needs Unicode rules.
	alts     []string
	altBytes [][]byte
	// lits are lower-cased needles for skipping raw bytes: a file or record that contains none of
	// them cannot match the term. nil when the term cannot be checked that way.
	lits [][]byte
}

// Matcher is a compiled query.
type Matcher struct {
	q       Query
	terms   []term
	fold    bool
	roles   map[ir.Role]bool
	agents  map[ir.SourceKind]bool
	project string
	// recordFilter: every term has lits, so records containing none of them can be skimmed.
	recordFilter bool
	// rawFirst: matches can be found in unredacted text and confirmed after redacting only the
	// candidate turns (literal terms that cannot touch a redaction marker or a word boundary).
	rawFirst bool
}

// Compile validates a query.
func Compile(q Query) (*Matcher, error) {
	if len(q.Patterns) == 0 {
		return nil, errors.New("no pattern given")
	}
	m := &Matcher{q: q, project: strings.ToLower(q.Project), fold: !q.CaseSensitive}
	m.recordFilter, m.rawFirst = true, !q.Regex && !q.Word
	for _, p := range q.Patterns {
		if p == "" {
			return nil, errors.New("empty pattern")
		}
		var t term
		expr := p
		if !q.Regex {
			var quoted []string
			safe, foldable := true, true
			for _, alt := range strings.Split(p, "|") {
				if alt == "" {
					continue
				}
				quoted = append(quoted, regexp.QuoteMeta(alt))
				low := alt
				if m.fold {
					low = strings.ToLower(alt)
					foldable = foldable && asciiFoldable([]byte(low))
				}
				t.alts = append(t.alts, low)
				t.altBytes = append(t.altBytes, []byte(low))
				t.lits = append(t.lits, []byte(strings.ToLower(alt)))
				safe = safe && skippable(alt)
			}
			if len(quoted) == 0 {
				return nil, fmt.Errorf("pattern %q has no text", p)
			}
			expr = "(?:" + strings.Join(quoted, "|") + ")"
			if !safe {
				t.lits = nil
			}
			if !foldable {
				t.alts = nil
			}
		} else if plain, err := regexp.Compile(p); err == nil {
			// A regex that starts with literal text (github\.com/…) can still be prefiltered on it.
			if prefix, _ := plain.LiteralPrefix(); len(prefix) >= 3 && skippable(prefix) {
				t.lits = [][]byte{[]byte(strings.ToLower(prefix))}
			}
		}
		if q.Word {
			expr = `\b(?:` + expr + `)\b`
		}
		if m.fold {
			expr = `(?i)` + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		t.re = re
		m.recordFilter = m.recordFilter && t.lits != nil
		m.rawFirst = m.rawFirst && t.lits != nil && t.alts != nil
		m.terms = append(m.terms, t)
	}
	if len(q.Roles) > 0 {
		m.roles = map[ir.Role]bool{}
		for _, r := range q.Roles {
			m.roles[r] = true
		}
	}
	if len(q.Agents) > 0 {
		m.agents = map[ir.SourceKind]bool{}
		for _, a := range q.Agents {
			m.agents[a] = true
		}
	}
	return m, nil
}

type syntheticRun struct {
	text    string   // lower-cased
	implies []string // words the source record contains whenever the run is rendered
}

var synthetic = func() []syntheticRun {
	out := make([]syntheticRun, len(sources.Synthetic))
	for i, s := range sources.Synthetic {
		out[i] = syntheticRun{strings.ToLower(s), sources.SyntheticImplies[s]}
	}
	return out
}()

// skippable reports whether raw bytes that lack s prove a rendered line cannot contain s: s
// appears unescaped in JSON (the agents write non-ASCII text as UTF-8, not \u escapes), has no
// whitespace (renderers collapse it), is not a bare number (truncation markers count characters)
// and cannot overlap text the renderers write themselves (sources.Synthetic).
func skippable(s string) bool {
	digits := true
	for _, r := range s {
		if r == '"' || r == '\\' || r < 0x20 || unicode.IsSpace(r) {
			return false
		}
		digits = digits && '0' <= r && r <= '9'
	}
	if digits {
		return false
	}
	a := strings.ToLower(s)
	for _, r := range synthetic {
		if overlaps(r.text, a) && !implied(r.implies, a) {
			return false
		}
	}
	return true
}

// overlaps reports whether a match of a can share bytes with run t.
func overlaps(t, a string) bool {
	if strings.Contains(t, a) || strings.Contains(a, t) {
		return true
	}
	for k := 1; k < len(a); k++ {
		if strings.HasSuffix(t, a[:k]) || strings.HasPrefix(t, a[k:]) {
			return true
		}
	}
	return false
}

// implied reports whether a is part of a word the record holds whenever the run is rendered.
func implied(words []string, a string) bool {
	for _, w := range words {
		if strings.Contains(w, a) {
			return true
		}
	}
	return false
}

// containsTerm reports whether raw holds one of a term's needles.
func containsTerm(raw []byte, alts [][]byte, lower *[]byte) bool {
	for _, a := range alts {
		if asciiFoldable(a) {
			if containsFoldASCII(raw, a) {
				return true
			}
			continue
		}
		if *lower == nil {
			*lower = bytes.ToLower(raw)
		}
		if bytes.Contains(*lower, a) {
			return true
		}
	}
	return false
}

// mayMatch is the cheap prefilter on a file's raw bytes. Rendered text is a redacted subset of
// what the file contains plus synthetic notation that skippable terms cannot touch, so a file
// without a term's needles cannot match it.
func (m *Matcher) mayMatch(raw []byte) bool {
	var lower []byte // only built for needles with cased non-ASCII letters
	if m.q.Any {
		for _, t := range m.terms {
			if t.lits == nil || containsTerm(raw, t.lits, &lower) {
				return true
			}
		}
		return false
	}
	for _, t := range m.terms {
		if t.lits != nil && !containsTerm(raw, t.lits, &lower) {
			return false
		}
	}
	return true
}

// keepRecord is the record filter for sources.ScanOptions.Keep: a record that renders no line of
// a searched role, or has none of the terms' needles, renders no matching line, so its content
// need not be decoded.
func (m *Matcher) keepRecord(rec []byte, roles []ir.Role) bool {
	if m.roles != nil {
		searched := false
		for _, r := range roles {
			searched = searched || m.roles[r]
		}
		if !searched {
			return false
		}
	}
	var lower []byte
	for _, t := range m.terms {
		if containsTerm(rec, t.lits, &lower) {
			return true
		}
	}
	return false
}

// asciiFoldable reports whether a lower-cased needle can be matched with ASCII-only case folding:
// it has no non-ASCII letters that have an upper-case form (CJK has none).
func asciiFoldable(needle []byte) bool {
	for _, r := range string(needle) {
		if r >= utf8.RuneSelf && unicode.SimpleFold(r) != r {
			return false
		}
	}
	return true
}

// containsFoldASCII reports whether hay contains the lower-case needle, ignoring ASCII case,
// without allocating. Like memchr-based searchers it anchors on the needle's rarest byte and
// jumps between its occurrences with bytes.IndexByte (vectorised), one pass per letter case.
func containsFoldASCII(hay, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(hay) {
		return false
	}
	p := rarest(needle)
	c := needle[p]
	if found(hay, needle, p, c) {
		return true
	}
	if 'a' <= c && c <= 'z' {
		return found(hay, needle, p, c-'a'+'A')
	}
	return false
}

// found looks for needle in hay at every occurrence of byte c, taken as needle[p].
func found(hay, needle []byte, p int, c byte) bool {
	last := len(hay) - len(needle) + p // the last offset needle[p] can have
	for i := p; i <= last; {
		j := bytes.IndexByte(hay[i:last+1], c)
		if j < 0 {
			return false
		}
		i += j
		if equalFoldASCII(hay[i-p:i-p+len(needle)], needle) {
			return true
		}
		i++
	}
	return false
}

// rarest picks the needle byte least likely in session logs: JSON punctuation, spaces, digits,
// UTF-8 continuation bytes and frequent English letters are poor anchors.
func rarest(needle []byte) int {
	best, bestRank := 0, 1<<30
	for i, c := range needle {
		if r := byteRank(c); r < bestRank {
			best, bestRank = i, r
		}
	}
	return best
}

const letterFreq = "etaoinsrhldcumfpgwybvkxjqz" // most to least frequent

func byteRank(c byte) int {
	switch {
	case 'a' <= c && c <= 'z':
		return 100 + 10*(len(letterFreq)-strings.IndexByte(letterFreq, c))
	case 'A' <= c && c <= 'Z':
		return 100 + 10*(len(letterFreq)-strings.IndexByte(letterFreq, c-'A'+'a'))
	case '0' <= c && c <= '9':
		return 300
	case c == ' ' || c == '"' || c == ',' || c == ':' || c == '{' || c == '}' || c == '\\' || c == '.' || c == '/' || c == '-' || c == '_':
		return 1000
	case c >= 0x80 && c < 0xc0:
		return 900 // continuation bytes: in every non-ASCII character
	case c >= 0xc0:
		return 500 // lead bytes: one per non-ASCII character, few distinct ones in CJK text
	}
	return 50
}

func equalFoldASCII(a, lower []byte) bool {
	for i, c := range a {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// prefilters reports whether reading raw bytes first can rule a file out.
func (m *Matcher) prefilters() bool {
	if m.q.Any {
		for _, t := range m.terms {
			if t.lits == nil {
				return false
			}
		}
		return true
	}
	for _, t := range m.terms {
		if t.lits != nil {
			return true
		}
	}
	return false
}

// scratch is per-worker memory for matching.
type scratch struct{ low []byte }

// find returns the matches of t in text, like t.re.FindAllStringIndex: leftmost first, then
// the first listed alternative, non-overlapping.
func (m *Matcher) find(t *term, text string, sc *scratch) [][2]int {
	if t.alts == nil || hasSpecialFold(text, m.fold) {
		locs := t.re.FindAllStringIndex(text, -1)
		out := make([][2]int, len(locs))
		for i, l := range locs {
			out[i] = [2]int{l[0], l[1]}
		}
		return out
	}
	// Search the line itself, or its ASCII-lower-cased copy (same length, so same offsets).
	index := func(from, ai int) int { return strings.Index(text[from:], t.alts[ai]) }
	if m.fold {
		sc.low = lowerASCII(sc.low[:0], text)
		index = func(from, ai int) int { return bytes.Index(sc.low[from:], t.altBytes[ai]) }
	}
	var out [][2]int
	for pos := 0; pos < len(text); {
		best, bestEnd := -1, 0
		for ai, a := range t.alts {
			for from := pos; from < len(text); {
				i := index(from, ai)
				if i < 0 {
					break
				}
				i += from
				if best >= 0 && i >= best {
					break // a listed-earlier alternative already starts here or before
				}
				if !m.q.Word || (wordBoundary(text, i) && wordBoundary(text, i+len(a))) {
					best, bestEnd = i, i+len(a)
					break
				}
				from = i + 1
			}
		}
		if best < 0 {
			break
		}
		out = append(out, [2]int{best, bestEnd})
		pos = bestEnd
	}
	return out
}

// hasSpecialFold reports text the regexp engine folds onto ASCII letters (KELVIN SIGN, LONG S),
// which the ASCII matcher does not; such rare lines go through the regexp.
func hasSpecialFold(text string, fold bool) bool {
	return fold && (strings.Contains(text, "K") || strings.Contains(text, "ſ"))
}

func lowerASCII(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}

func isWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// wordBoundary is RE2's ASCII \b at byte offset i.
func wordBoundary(s string, i int) bool {
	before := i > 0 && isWordByte(s[i-1])
	after := i < len(s) && isWordByte(s[i])
	return before != after
}

// match finds the patterns in a turn's searchable lines (Elided lines have no text).
func (m *Matcher) match(t Turn, sc *scratch) ([][][2]int, bool) {
	var matched [][][2]int
	hitPattern := make([]bool, len(m.terms))
	for i, l := range t.Lines {
		if l.Elided || m.roles != nil && !m.roles[l.Role] {
			continue
		}
		for pi := range m.terms {
			locs := m.find(&m.terms[pi], l.Text, sc)
			if len(locs) == 0 {
				continue
			}
			hitPattern[pi] = true
			if matched == nil {
				matched = make([][][2]int, len(t.Lines))
			}
			matched[i] = append(matched[i], locs...)
		}
	}
	any, all := false, true
	for _, h := range hitPattern {
		any = any || h
		all = all && h
	}
	if matched == nil {
		matched = make([][][2]int, len(t.Lines))
	}
	if m.q.Any {
		return matched, any
	}
	return matched, all
}

// SplitTurns groups a session's lines into turns, numbered from 1.
func SplitTurns(lines []ir.Line) []Turn {
	var turns []Turn
	for _, l := range lines {
		if l.TurnStart || len(turns) == 0 {
			turns = append(turns, Turn{Index: len(turns) + 1, TS: l.TS})
		}
		t := &turns[len(turns)-1]
		t.Lines = append(t.Lines, l)
		if t.TS == "" {
			t.TS = l.TS
		}
	}
	return turns
}

// displayPath is where a session lives: its file, or database#session for sessions inside one.
func displayPath(f ir.SourceFile) string {
	if f.Inner != "" {
		return f.Key
	}
	return f.Path
}

// Info describes a parsed session.
func Info(s *ir.Session, turns int) SessionInfo {
	return SessionInfo{Agent: s.Kind, ID: s.SessionID, Path: displayPath(s.File), Key: s.File.Key, Cwd: s.Cwd, Title: s.Title, StartedAt: s.StartedAt, Subagent: s.File.AgentName, Turns: turns}
}

func (m *Matcher) inWindow(ts string) bool {
	if m.q.Since.IsZero() && m.q.Until.IsZero() {
		return true
	}
	t, ok := util.ParseTime(ts)
	if !ok {
		return false
	}
	return (m.q.Since.IsZero() || !t.Before(m.q.Since)) && (m.q.Until.IsZero() || t.Before(m.q.Until))
}

// worker is one search goroutine's reusable state.
type worker struct {
	buf []byte
	sc  scratch
}

// fileResult is what searching one file found.
type fileResult struct {
	hits                    []Hit
	skipped, parsed, nturns int
}

func (m *Matcher) searchFile(f ir.SourceFile, w *worker) (fileResult, error) {
	var r fileResult
	if m.agents != nil && !m.agents[f.Kind] {
		return r, nil
	}
	// A session last written before Since has no turn after it.
	if !m.q.Since.IsZero() && f.MtimeMs < float64(m.q.Since.UnixNano())/1e6 {
		r.skipped = 1
		return r, nil
	}
	raw, err := sources.ReadFileInto(f, w.buf)
	if err != nil {
		return r, err
	}
	if cap(raw) <= 64<<20 {
		w.buf = raw[:0] // keep the buffer for the next file, unless it is huge
	}
	if m.prefilters() && !m.mayMatch(raw) {
		r.skipped = 1
		return r, nil
	}
	opt := sources.ScanOptions{Raw: m.rawFirst}
	// Skimming pays when few hits come back whole. When all of them do (-n 0), a file with a hit
	// would be parsed twice, so it is decoded once in full.
	if m.recordFilter && (m.q.Limit > 0 || m.q.Partial) {
		opt.Keep = m.keepRecord
	}
	s, err := sources.OpenBytes(f, raw, opt)
	if err != nil || s == nil || s.SkipReason != "" {
		return r, err
	}
	if m.project != "" && !strings.Contains(strings.ToLower(s.Cwd), m.project) {
		return r, nil
	}
	turns := SplitTurns(s.Lines(0))
	r.parsed, r.nturns = 1, len(turns)
	info := Info(s, len(turns))
	for _, t := range turns {
		if !m.inWindow(t.TS) {
			continue
		}
		matched, ok := m.match(t, &w.sc)
		if !ok {
			continue
		}
		partial := false
		if m.rawFirst || opt.Keep != nil {
			if m.rawFirst {
				// Confirm on the redacted text: a secret must never be found.
				t = redactTurn(t)
				if matched, ok = m.match(t, &w.sc); !ok {
					continue
				}
			}
			for _, l := range t.Lines {
				partial = partial || l.Elided
			}
		}
		r.hits = append(r.hits, Hit{Session: info, Turn: t, Matched: matched, file: f, partial: partial})
	}
	return r, nil
}

func redactTurn(t Turn) Turn {
	lines := make([]ir.Line, len(t.Lines))
	for i, l := range t.Lines {
		if !l.Elided {
			l.Text = util.Redact(l.Text)
		}
		lines[i] = l
	}
	t.Lines = lines
	return t
}

// earlyStop decides when the remaining (older) files can no longer contribute to the Limit
// newest hits: a session file is written after each of its turns, so no turn in it is newer than
// its mtime. A minute of slack covers clock and timestamp rounding.
type earlyStop struct {
	limit  int
	newest newestTS // the limit+1 newest hit times seen so far
}

func (e *earlyStop) add(hits []Hit) {
	for _, h := range hits {
		e.newest.push(h.Turn.TS, e.limit+1)
	}
}

// done reports whether a file with this mtime and every older one can be left unread: limit+1
// hits newer than it exist, so the results are full and known to be cut.
func (e *earlyStop) done(mtimeMs float64) bool {
	if e.limit <= 0 || len(e.newest) <= e.limit {
		return false
	}
	t, ok := util.ParseTime(e.newest.oldest())
	return ok && float64(t.UnixNano())/1e6 > mtimeMs+60_000
}

// newestTS keeps the n newest timestamps (a min-heap on the string form, which sorts like the
// ISO 8601 times the agents write).
type newestTS []string

func (h *newestTS) push(ts string, n int) {
	if ts == "" {
		return
	}
	if len(*h) < n {
		*h = append(*h, ts)
		h.up(len(*h) - 1)
		return
	}
	if ts > (*h)[0] {
		(*h)[0] = ts
		h.down(0)
	}
}

func (h newestTS) oldest() string { return h[0] }

func (h newestTS) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if h[p] <= h[i] {
			return
		}
		h[p], h[i] = h[i], h[p]
		i = p
	}
}

func (h newestTS) down(i int) {
	for {
		l, small := 2*i+1, i
		if l < len(h) && h[l] < h[small] {
			small = l
		}
		if r := l + 1; r < len(h) && h[r] < h[small] {
			small = r
		}
		if small == i {
			return
		}
		h[i], h[small] = h[small], h[i]
		i = small
	}
}

// Search scans files in parallel, newest first, and returns matching turns, newest first. With
// a Limit it returns at most Limit hits and stops reading once older files cannot change them
// (Stats.Complete then reports false). Unless q.Partial, every returned hit has its whole turn.
func Search(files []ir.SourceFile, q Query) ([]Hit, Stats, error) {
	start := time.Now()
	m, err := Compile(q)
	if err != nil {
		return nil, Stats{}, err
	}
	order := make([]ir.SourceFile, len(files))
	copy(order, files)
	sort.SliceStable(order, func(i, j int) bool { return order[i].MtimeMs > order[j].MtimeMs })

	st := Stats{Files: len(files), Complete: true}
	var (
		mu    sync.Mutex
		hits  []Hit
		first error
		wg    sync.WaitGroup
		stop  = earlyStop{limit: q.Limit}
	)
	jobs := make(chan ir.SourceFile)
	var deadline time.Time
	if q.Timeout > 0 {
		deadline = start.Add(q.Timeout)
	}
	expired := func() bool { return !deadline.IsZero() && time.Now().After(deadline) }
	for i := 0; i < workers(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &worker{}
			for f := range jobs {
				if expired() { // handed over just before the time ran out
					mu.Lock()
					st.Unread++
					st.Skipped++
					st.TimedOut = true
					mu.Unlock()
					continue
				}
				r, err := m.searchFile(f, w)
				mu.Lock()
				if err != nil && first == nil {
					first = fmt.Errorf("%s: %w", f.Path, err)
				}
				hits = append(hits, r.hits...)
				stop.add(r.hits)
				st.Skipped += r.skipped
				st.Parsed += r.parsed
				st.Sessions += r.parsed
				st.Turns += r.nturns
				mu.Unlock()
			}
		}()
	}
	unread, timedOut := 0, false
	for i, f := range order {
		mu.Lock()
		done := stop.done(f.MtimeMs)
		mu.Unlock()
		if done {
			unread = len(order) - i
			break
		}
		if expired() {
			unread = len(order) - i
			timedOut = true
			break
		}
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	st.Unread += unread
	st.Skipped += unread
	st.TimedOut = st.TimedOut || timedOut
	if st.Unread > 0 {
		st.Complete = false
	}
	SortNewestFirst(hits)
	st.Matched = len(hits)
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Session.Key] = true
	}
	st.MatchedSessions = len(seen)
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	if !q.Partial {
		if err := m.complete(hits); err != nil && first == nil {
			first = err
		}
	}
	st.Elapsed = time.Since(start)
	return hits, st, first
}

// workers is the number of search goroutines: reading is cheap next to parsing, so one per CPU.
func workers() int { return runtime.GOMAXPROCS(0) }

// complete replaces partial hits by their whole turns, reading each of their sessions once more,
// in parallel.
func (m *Matcher) complete(hits []Hit) error {
	byFile := map[string][]int{}
	var paths []string
	for i, h := range hits {
		if h.partial {
			if byFile[h.file.Key] == nil {
				paths = append(paths, h.file.Key)
			}
			byFile[h.file.Key] = append(byFile[h.file.Key], i)
		}
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	jobs := make(chan []int)
	for w := 0; w < workers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var sc scratch
			var buf []byte
			for idx := range jobs {
				f := hits[idx[0]].file
				raw, err := sources.ReadFileInto(f, buf)
				var s *ir.Session
				if err == nil {
					if cap(raw) <= 64<<20 {
						buf = raw[:0]
					}
					s, err = sources.OpenBytes(f, raw, sources.ScanOptions{})
				}
				if err != nil || s == nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %v", f.Path, err)
					}
					mu.Unlock()
					continue
				}
				turns := SplitTurns(s.Lines(0))
				for _, i := range idx { // each hit is written by one goroutine only
					h := &hits[i]
					if h.Turn.Index > len(turns) {
						continue
					}
					t := turns[h.Turn.Index-1]
					matched, _ := m.match(t, &sc)
					h.Turn, h.Matched, h.partial = t, matched, false
				}
			}
		}()
	}
	for _, p := range paths {
		jobs <- byFile[p]
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

// SortNewestFirst orders hits by turn time (newest first), then session and turn.
func SortNewestFirst(hits []Hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Turn.TS != b.Turn.TS {
			return a.Turn.TS > b.Turn.TS
		}
		if a.Session.Key != b.Session.Key {
			return a.Session.Key < b.Session.Key
		}
		return a.Turn.Index < b.Turn.Index
	})
}

var relRe = regexp.MustCompile(`^(\d+)([hdwmy])$`)

// ParseTimeBound accepts a relative age ("36h", "7d", "2w", "3m", "1y") or a date / timestamp
// ("2026-10-01", RFC 3339) and returns the point in time it names.
func ParseTimeBound(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if m := relRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "h":
			return now.Add(-time.Duration(n) * time.Hour), nil
		case "d":
			return now.AddDate(0, 0, -n), nil
		case "w":
			return now.AddDate(0, 0, -7*n), nil
		case "m":
			return now.AddDate(0, -n, 0), nil
		default:
			return now.AddDate(-n, 0, 0), nil
		}
	}
	if t, ok := util.ParseTime(s); ok {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot read time %q (use 7d, 36h, 2w, 3m, 1y or 2026-10-01)", s)
}
