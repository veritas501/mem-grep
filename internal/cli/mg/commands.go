package mg

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/veritas501/mem-grep/internal/grep"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// outLine is one printed line of a turn.
type outLine struct {
	Role  ir.Role  `json:"role"`
	Text  string   `json:"text"`
	Hit   bool     `json:"hit,omitempty"`
	Match [][2]int `json:"match,omitempty"` // byte offsets into Text
}

// selectLines picks what to print of a hit: the turn's opening line (the user message or task,
// as an anchor), matched lines and `context` lines around them — or everything with full.
// selectLines picks what to print of a matching turn: matched lines with context (or everything
// with full), restricted to roles when given, plus the turn's opening line as an anchor.
func selectLines(h grep.Hit, context int, full bool, width int, roles map[ir.Role]bool) []outLine {
	n := len(h.Turn.Lines)
	keep := make([]bool, n)
	for i := range h.Turn.Lines {
		if full || len(h.Matched[i]) > 0 {
			for j := max(0, i-context); j <= min(n-1, i+context); j++ {
				keep[j] = roles == nil || roles[h.Turn.Lines[j].Role]
			}
		}
	}
	if n > 0 {
		keep[0] = true // the turn's opening line (user message, task, branch note) anchors it
	}
	if full {
		width = 0
	}
	var out []outLine
	for i, l := range h.Turn.Lines {
		if !keep[i] {
			continue
		}
		text, ranges := grep.Snippet(strings.TrimLeft(l.Text, " "), shift(h.Matched[i], len(l.Text)-len(strings.TrimLeft(l.Text, " "))), width)
		out = append(out, outLine{Role: l.Role, Text: text, Hit: len(h.Matched[i]) > 0, Match: ranges})
	}
	return out
}

func shift(rs [][2]int, by int) [][2]int {
	out := make([][2]int, 0, len(rs))
	for _, r := range rs {
		if r[0] >= by {
			out = append(out, [2]int{r[0] - by, r[1] - by})
		}
	}
	return out
}

func header(s grep.SessionInfo) string {
	h := util.ShortPath(s.Cwd)
	if s.Subagent != "" {
		h += "  subagent:" + s.Subagent
	}
	return h
}

type hitJSON struct {
	Type     string        `json:"type"`
	Ref      string        `json:"ref"`
	Agent    ir.SourceKind `json:"agent"`
	Session  string        `json:"session"`
	Turn     int           `json:"turn"`
	Turns    int           `json:"turns"`
	TS       string        `json:"ts"`
	Cwd      string        `json:"cwd"`
	Subagent string        `json:"subagent,omitempty"`
	Path     string        `json:"path"`
	Lines    []outLine     `json:"lines"`
}

type summaryJSON struct {
	Type      string `json:"type"`
	Matched   int    `json:"matched"`
	Shown     int    `json:"shown"`
	Sessions  int    `json:"sessions"`
	Searched  int    `json:"searched_files"`
	Parsed    int    `json:"parsed_files"`
	Skipped   int    `json:"skipped_files"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Truncated bool   `json:"truncated"`
	// Complete is false when the search stopped early (matched is then a lower bound).
	Complete bool `json:"complete"`
	// TimedOut: --timeout stopped the search; a result without matches is then inconclusive.
	TimedOut bool `json:"timed_out,omitempty"`
	// BudgetExhausted: --max-chars stopped the printing; truncated is true and shown < matched.
	BudgetExhausted bool   `json:"budget_exhausted,omitempty"`
	Next            string `json:"next,omitempty"`
}

// showSummary ends the JSON Lines of one session printed by `mg show`.
type showSummary struct {
	Type            string `json:"type"`
	Ref             string `json:"ref"`
	From            int    `json:"from"`
	To              int    `json:"to"`
	Turns           int    `json:"turns"`
	BudgetExhausted bool   `json:"budget_exhausted,omitempty"`
	Next            string `json:"next,omitempty"`
}

// hitChars is the size of a printed hit in characters (header and lines, as printed).
func hitChars(h grep.Hit, lines []outLine) int {
	n := utf8.RuneCountInString(h.Ref()) + utf8.RuneCountInString(header(h.Session)) + 40
	for _, l := range lines {
		n += utf8.RuneCountInString(l.Text) + 3
	}
	return n
}

func runSearch(cmd *cobra.Command, o *options, terms []string) error {
	q, err := o.query(terms)
	if err != nil {
		return err
	}
	files, err := o.files()
	if err != nil {
		return err
	}
	p, err := newPrinter(cmd, o)
	if err != nil {
		return err
	}
	q.Timeout = o.timeout
	if o.top {
		q.Partial = true // --top only needs the matched lines
	} else {
		q.Limit = o.limit // lets the search stop once older sessions cannot make the list
	}
	hits, st, err := grep.Search(files, q)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "mg: warning: "+err.Error())
	}
	sessions := map[string]bool{}
	for _, h := range hits {
		sessions[h.Session.Key] = true
	}
	stats := fmt.Sprintf("searched %s in %dms", plural(st.Files, "session file"), st.Elapsed.Milliseconds())
	switch {
	case st.TimedOut:
		stats = fmt.Sprintf("TIMED OUT after %s: examined %d of %s, newest first", o.timeout, st.Files-st.Unread, plural(st.Files, "session file"))
	case !st.Complete:
		stats = fmt.Sprintf("examined %d of %s in %dms, newest first (older ones cannot outrank these)", st.Files-st.Unread, plural(st.Files, "session file"), st.Elapsed.Milliseconds())
	}
	if len(hits) == 0 {
		if p.json {
			p.emit(summaryJSON{Type: "summary", Searched: st.Files, Parsed: st.Parsed, Skipped: st.Skipped, ElapsedMs: st.Elapsed.Milliseconds(), Complete: st.Complete, TimedOut: st.TimedOut})
		} else if st.TimedOut {
			fmt.Fprintf(p.w, "-- inconclusive: nothing found in what was read, but %s\n", stats)
			fmt.Fprintln(p.w, "-- this does not mean nothing matches: raise --timeout, or search less with --since, -p, -a, -r and distinctive terms")
		} else {
			fmt.Fprintf(p.w, "-- no matches; %s\n", stats)
			fmt.Fprintln(p.w, "-- try: fewer terms, --any, alternatives 'a|b' (other wording, the other language), -e for a regex, a wider --since")
		}
		if st.TimedOut {
			return errInconclusive
		}
		return errNoMatch
	}

	if o.top {
		return printTop(p, o, hits, sessions, stats, st)
	}

	shown := hits
	if o.limit > 0 && len(shown) > o.limit {
		shown = shown[:o.limit]
	}
	used, budgetCut := 0, false
	var printed []grep.Hit
	for _, h := range shown {
		lines := selectLines(h, o.context, o.full, o.width, o.roleSet())
		size := hitChars(h, lines)
		if o.maxChars > 0 && len(printed) > 0 && used+size > o.maxChars {
			budgetCut = true // the first hit is always printed, so an answer never comes back empty
			break
		}
		used += size
		printed = append(printed, h)
		if p.json {
			p.emit(hitJSON{Type: "hit", Ref: h.Ref(), Agent: h.Session.Agent, Session: h.Session.ID, Turn: h.Turn.Index, Turns: h.Session.Turns,
				TS: h.Turn.TS, Cwd: h.Session.Cwd, Subagent: h.Session.Subagent, Path: h.Session.Path, Lines: lines})
			continue
		}
		fmt.Fprintf(p.w, "%s  %s  %s  (turn %d/%d)\n", h.Ref(), short(h.Turn.TS), header(h.Session), h.Turn.Index, h.Session.Turns)
		for _, l := range lines {
			mark := "  "
			if l.Hit {
				mark = "> "
			}
			fmt.Fprintln(p.w, mark+p.highlight(l.Text, l.Match))
		}
		fmt.Fprintln(p.w)
	}
	shown = printed
	next := "mg show " + shown[0].Ref() + " -B 1 -A 2"
	if p.json {
		p.emit(summaryJSON{Type: "summary", Matched: st.Matched, Shown: len(shown), Sessions: st.MatchedSessions, Searched: st.Files, Parsed: st.Parsed,
			Skipped: st.Skipped, ElapsedMs: st.Elapsed.Milliseconds(), Truncated: len(shown) < st.Matched, Complete: st.Complete, TimedOut: st.TimedOut, BudgetExhausted: budgetCut, Next: next})
		return nil
	}
	atLeast := ""
	if !st.Complete {
		atLeast = "at least "
	}
	fmt.Fprintf(p.w, "-- %s%s in %s matched, newest first; %s\n", atLeast, plural(st.Matched, "turn"), plural(st.MatchedSessions, "session"), stats)
	if st.TimedOut {
		fmt.Fprintln(p.w, "-- older sessions were not searched: raise --timeout, or search less with --since, -p, -a, -r")
	}
	if budgetCut {
		fmt.Fprintf(p.w, "-- output budget of %d characters reached: showing %d; %s%d more: raise --max-chars, or narrow the query\n", o.maxChars, len(shown), atLeast, st.Matched-len(shown))
	} else if len(shown) < st.Matched {
		fmt.Fprintf(p.w, "-- showing %d; %s%d more: raise -n, or narrow with -w (whole words), more terms, --since, -p, -r\n", len(shown), atLeast, st.Matched-len(shown))
	}
	fmt.Fprintf(p.w, "-- read it in full with the turn before and two after: %s\n", next)
	return nil
}

func printTop(p *printer, o *options, hits []grep.Hit, sessions map[string]bool, stats string, st grep.Stats) error {
	entries := grep.Top(hits, !o.caseSens)
	shown := entries
	if o.limit > 0 && len(shown) > o.limit {
		shown = shown[:o.limit]
	}
	if p.json {
		for _, e := range shown {
			p.emit(struct {
				Type string `json:"type"`
				grep.TopEntry
			}{"top", e})
		}
		p.emit(summaryJSON{Type: "summary", Matched: len(hits), Shown: len(shown), Sessions: len(sessions), Searched: st.Files, Parsed: st.Parsed,
			Skipped: st.Skipped, ElapsedMs: st.Elapsed.Milliseconds(), Truncated: len(shown) < len(entries), Complete: st.Complete, TimedOut: st.TimedOut})
		return nil
	}
	fmt.Fprintf(p.w, "%8s  %8s  %-16s  %-24s  %s\n", "sessions", "count", "last", "example", "match")
	for _, e := range shown {
		fmt.Fprintf(p.w, "%8d  %8d  %-16s  %-24s  %s\n", e.Sessions, e.Count, short(e.Last), e.Example, e.Match)
	}
	fmt.Fprintf(p.w, "-- %s, ranked by sessions then count, from %s in %s; %s\n", plural(len(entries), "distinct match"), plural(len(hits), "turn"), plural(len(sessions), "session"), stats)
	if len(shown) < len(entries) {
		fmt.Fprintf(p.w, "-- showing %d; raise -n for more\n", len(shown))
	}
	if st.TimedOut {
		fmt.Fprintln(p.w, "-- counts cover only the newer sessions: raise --timeout for the full picture")
	}
	fmt.Fprintln(p.w, "-- see a use in context: mg show <example>")
	return nil
}

func runShow(cmd *cobra.Command, o *options, refs []string) error {
	if _, err := o.parseRoles(); err != nil {
		return err
	}
	files, err := o.files()
	if err != nil {
		return err
	}
	p, err := newPrinter(cmd, o)
	if err != nil {
		return err
	}
	width := o.width
	if o.full || !cmd.Flags().Changed("width") {
		width = 0 // show prints turns in full unless asked otherwise
	}
	roles := o.roleSet()
	for i, arg := range refs {
		r, err := grep.ParseRef(arg)
		if err != nil {
			return err
		}
		s, err := grep.Resolve(files, r)
		if err != nil {
			return err
		}
		turns := grep.SplitTurns(s.Lines(0))
		info := grep.Info(s, len(turns))
		from, to := 1, len(turns)
		if r.From > 0 {
			from, to = r.From, r.To
		}
		from, to = max(1, from-o.before), min(len(turns), to+o.after)
		if r.From == 0 && o.limit > 0 && to-from+1 > o.limit {
			to = from + o.limit - 1 // a whole session: the first -n turns, the footer says how to go on
		}
		if from > len(turns) {
			return fmt.Errorf("%s has %s", info.Ref(), plural(len(turns), "turn"))
		}
		if p.json {
			p.emit(struct {
				Type string `json:"type"`
				Ref  string `json:"ref"`
				grep.SessionInfo
			}{"session", info.Ref(), info})
		} else {
			if i > 0 {
				fmt.Fprintln(p.w)
			}
			last := ""
			if len(turns) > 0 {
				last = " → " + short(turns[len(turns)-1].TS)
			}
			fmt.Fprintf(p.w, "%s  %s  %s%s  %s\n", info.Ref(), header(info), short(info.StartedAt), last, plural(len(turns), "turn"))
			if info.Title != "" {
				fmt.Fprintln(p.w, "title: "+info.Title)
			}
			fmt.Fprintln(p.w, "file: "+info.Path)
		}
		used, budgetCut := 0, false
		for _, t := range turns[from-1 : to] {
			var lines []outLine
			hidden := 0
			for j, l := range t.Lines {
				if j > 0 && roles != nil && !roles[l.Role] {
					hidden++ // the opening line stays as the anchor
					continue
				}
				text, _ := grep.Snippet(l.Text, nil, width)
				lines = append(lines, outLine{Role: l.Role, Text: text})
			}
			if hidden > 0 && !p.json {
				lines = append(lines, outLine{Role: ir.RoleNote, Text: fmt.Sprintf("  [%s of other roles not shown]", plural(hidden, "line"))})
			}
			size := 20
			for _, l := range lines {
				size += utf8.RuneCountInString(l.Text) + 3
			}
			if o.maxChars > 0 && t.Index > from && used+size > o.maxChars {
				budgetCut = true // the first turn is always printed
				to = t.Index - 1
				break
			}
			used += size
			if p.json {
				p.emit(struct {
					Type  string    `json:"type"`
					Ref   string    `json:"ref"`
					Turn  int       `json:"turn"`
					TS    string    `json:"ts"`
					Lines []outLine `json:"lines"`
				}{"turn", fmt.Sprintf("%s#%d", info.Ref(), t.Index), t.Index, t.TS, lines})
				continue
			}
			fmt.Fprintf(p.w, "\n#%d  %s\n", t.Index, short(t.TS))
			for _, l := range lines {
				fmt.Fprintln(p.w, l.Text)
			}
		}
		if p.json {
			sum := showSummary{Type: "summary", Ref: info.Ref(), From: from, To: to, Turns: len(turns), BudgetExhausted: budgetCut}
			if to < len(turns) {
				sum.Next = fmt.Sprintf("mg show %s#%d-%d", info.Ref(), to+1, min(len(turns), to+(to-from+1)))
			}
			p.emit(sum)
		} else {
			fmt.Fprintf(p.w, "-- turns %d-%d of %d", from, to, len(turns))
			if budgetCut {
				fmt.Fprintf(p.w, "; output budget of %d characters reached", o.maxChars)
			}
			if to < len(turns) {
				fmt.Fprintf(p.w, "; next: mg show %s#%d-%d", info.Ref(), to+1, min(len(turns), to+(to-from+1)))
			}
			fmt.Fprintln(p.w)
		}
	}
	return nil
}

func runSessions(cmd *cobra.Command, o *options, terms []string) error {
	files, err := o.files()
	if err != nil {
		return err
	}
	q, err := o.query(terms)
	if err != nil {
		return err
	}
	p, err := newPrinter(cmd, o)
	if err != nil {
		return err
	}
	hitCount := map[string]int{}
	if len(terms) > 0 {
		q.Partial = true // only counted
		hits, _, err := grep.Search(files, q)
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "mg: warning: "+err.Error())
		}
		var keep []ir.SourceFile
		for _, h := range hits {
			hitCount[h.Session.Key]++
		}
		for _, f := range files {
			if hitCount[f.Key] > 0 {
				keep = append(keep, f)
			}
		}
		files = keep
		q.Since, q.Until = time.Time{}, time.Time{} // already applied to the turns
	}
	lq := q
	lq.Limit = 0
	ls, err := grep.List(files, lq)
	if err != nil {
		return err
	}
	if len(ls) == 0 {
		if !p.json {
			fmt.Fprintln(p.w, "-- no sessions")
		}
		return errNoMatch
	}
	shown := ls
	if o.limit > 0 && len(shown) > o.limit {
		shown = shown[:o.limit]
	}
	for _, l := range shown {
		first, _ := grep.Snippet(strings.TrimPrefix(strings.TrimPrefix(l.First, "USER: "), "TASK for "), nil, min(o.width, 160))
		if p.json {
			p.emit(struct {
				Type string `json:"type"`
				Ref  string `json:"ref"`
				grep.Listing
				Hits int `json:"hits,omitempty"`
			}{"session", l.Ref(), l, hitCount[l.Key]})
			continue
		}
		hits := ""
		if n := hitCount[l.Key]; n > 0 {
			hits = "  " + plural(n, "hit")
		}
		fmt.Fprintf(p.w, "%s  %s  %s  %s%s  %s\n", l.Ref(), short(l.LastTS), header(l.SessionInfo), plural(l.Turns, "turn"), hits, first)
	}
	if !p.json {
		fmt.Fprintf(p.w, "-- %s, most recent activity first", plural(len(ls), "session"))
		if len(shown) < len(ls) {
			fmt.Fprintf(p.w, "; showing %d (raise -n)", len(shown))
		}
		fmt.Fprintf(p.w, "; read one: mg show %s\n", shown[0].Ref())
	}
	return nil
}
