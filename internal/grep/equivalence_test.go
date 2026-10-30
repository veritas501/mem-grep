package grep

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/sources"
	"github.com/veritas501/mem-grep/internal/testutil"
)

// referenceSearch is the plain search the optimised one must agree with: every file is read in
// full and every line is matched with the regexps. No prefilter, no skimming, no early stop.
func referenceSearch(t *testing.T, files []ir.SourceFile, q Query) []Hit {
	t.Helper()
	m, err := Compile(q)
	if err != nil {
		t.Fatal(err)
	}
	var hits []Hit
	for _, f := range files {
		if m.agents != nil && !m.agents[f.Kind] {
			continue
		}
		s, err := sources.Open(f)
		if err != nil || s == nil || s.SkipReason != "" {
			continue
		}
		if m.project != "" && !strings.Contains(strings.ToLower(s.Cwd), m.project) {
			continue
		}
		turns := SplitTurns(s.Lines(0))
		for _, tn := range turns {
			if !m.inWindow(tn.TS) {
				continue
			}
			matched := make([][][2]int, len(tn.Lines))
			hit := make([]bool, len(m.terms))
			for i, l := range tn.Lines {
				if m.roles != nil && !m.roles[l.Role] {
					continue
				}
				for pi, term := range m.terms {
					for _, loc := range term.re.FindAllStringIndex(l.Text, -1) {
						matched[i] = append(matched[i], [2]int{loc[0], loc[1]})
						hit[pi] = true
					}
				}
			}
			any, all := false, true
			for _, h := range hit {
				any, all = any || h, all && h
			}
			if (q.Any && any) || (!q.Any && all) {
				hits = append(hits, Hit{Session: Info(s, len(turns)), Turn: tn, Matched: matched})
			}
		}
	}
	SortNewestFirst(hits)
	return hits
}

// equivalenceQueries cover every path of the engine: literals with and without folding and word
// boundaries, alternatives, CJK, regexes with and without a literal prefix, --any, roles,
// projects, time windows, and terms that touch text the renderers write themselves (which must
// disable skipping) or redaction markers.
func equivalenceQueries() []Query {
	return []Query{
		{Patterns: []string{"econnrefused"}},
		{Patterns: []string{"econnrefused", "mock"}},
		{Patterns: []string{"ECONNREFUSED"}, CaseSensitive: true},
		{Patterns: []string{"test-db|数据库"}},
		{Patterns: []string{"docker", "网络|network", "修|fix|恢复|好了"}},
		{Patterns: []string{"cli"}, Word: true},
		{Patterns: []string{"cobra|cli", "log|日志"}, Word: true, Roles: []ir.Role{ir.RoleUser}},
		{Patterns: []string{"pnpm", "test"}, Any: true},
		{Patterns: []string{`github\.com/[\w.-]+/[\w.-]+`}, Regex: true, Roles: []ir.Role{ir.RoleTool, ir.RoleResult}},
		{Patterns: []string{`\d{4,}`}, Regex: true},
		{Patterns: []string{`(?i)iptables-(nft|legacy)`}, Regex: true},
		{Patterns: []string{"USER:"}},
		{Patterns: []string{"failed:"}},
		{Patterns: []string{"bash"}, Roles: []ir.Role{ir.RoleResult}},
		{Patterns: []string{"tool"}},
		{Patterns: []string{"redacted"}},
		{Patterns: []string{"chars omitted"}},
		{Patterns: []string{"2026"}},
		{Patterns: []string{"exit"}, Roles: []ir.Role{ir.RoleShell}},
		{Patterns: []string{"branch"}},
		{Patterns: []string{"the", "a"}},
		{Patterns: []string{"e"}, Limit: 5},
		{Patterns: []string{"error|错误"}, Limit: 3},
		{Patterns: []string{"readme"}, Project: "shop"},
		{Patterns: []string{"a"}, Since: time.Now().AddDate(-1, 0, 0)},
	}
}

func sameHits(t *testing.T, label string, got, want []Hit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d hits, reference %d", label, len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.Ref() != w.Ref() || g.Session.Turns != w.Session.Turns || g.Turn.TS != w.Turn.TS {
			t.Fatalf("%s: hit %d is %s (%d turns, %s), reference %s (%d turns, %s)", label, i, g.Ref(), g.Session.Turns, g.Turn.TS, w.Ref(), w.Session.Turns, w.Turn.TS)
		}
		if !reflect.DeepEqual(g.Turn.Lines, w.Turn.Lines) {
			t.Fatalf("%s: %s lines differ:\n got %q\nwant %q", label, g.Ref(), ir.Text(g.Turn.Lines), ir.Text(w.Turn.Lines))
		}
		if !reflect.DeepEqual(g.Matched, w.Matched) {
			t.Fatalf("%s: %s matches differ:\n got %v\nwant %v", label, g.Ref(), g.Matched, w.Matched)
		}
	}
}

func checkEquivalence(t *testing.T, files []ir.SourceFile, queries []Query) {
	for _, q := range queries {
		label := fmt.Sprintf("%q regex=%v case=%v word=%v any=%v roles=%v limit=%d", q.Patterns, q.Regex, q.CaseSensitive, q.Word, q.Any, q.Roles, q.Limit)
		want := referenceSearch(t, files, q)
		got, st, err := Search(files, q)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if q.Limit > 0 && len(want) > q.Limit {
			want = want[:q.Limit]
			if st.Matched <= q.Limit {
				t.Fatalf("%s: matched %d, but more than %d exist", label, st.Matched, q.Limit)
			}
		} else if st.Matched != len(want) || !st.Complete {
			t.Fatalf("%s: matched %d complete=%v, reference %d", label, st.Matched, st.Complete, len(want))
		}
		sameHits(t, label, got, want)
	}
}

func TestSearchEqualsReference(t *testing.T) {
	checkEquivalence(t, fixtureFiles(t), equivalenceQueries())
}

// TestSearchEqualsReferenceOnCorpus runs the same comparison on a large corpus of sessions that
// omp itself generated (MG_CORPUS=<dir>; see docs/development.md). Skipped otherwise.
func TestSearchEqualsReferenceOnCorpus(t *testing.T) {
	dir := os.Getenv("MG_CORPUS")
	if dir == "" {
		t.Skip("set MG_CORPUS to a directory of generated sessions")
	}
	files, err := sources.FilesUnder(dir)
	if err != nil {
		t.Fatal(err)
	}
	checkEquivalence(t, files, equivalenceQueries())
}

// TestLiteralFindEqualsRegexp compares the literal matcher with the regexp it replaces on random
// text: same matches, same offsets, with folding, word boundaries and overlapping alternatives.
func TestLiteralFindEqualsRegexp(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pieces := []string{"a", "b", "ab", "A", "B", "_", " ", "-", "cli", "CLI", "client", "数据", "库", "é", "É", "K", "k", "K", "1", "\n"}
	gen := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		return b.String()
	}
	for i := 0; i < 3000; i++ {
		var alts []string
		for j := 0; j <= rng.Intn(3); j++ {
			if a := strings.TrimSpace(gen(1 + rng.Intn(3))); a != "" && !strings.Contains(a, "|") {
				alts = append(alts, a)
			}
		}
		if len(alts) == 0 {
			continue
		}
		q := Query{Patterns: []string{strings.Join(alts, "|")}, CaseSensitive: rng.Intn(2) == 0, Word: rng.Intn(2) == 0}
		m, err := Compile(q)
		if err != nil {
			t.Fatal(err)
		}
		tm := &m.terms[0]
		text := gen(rng.Intn(30))
		var want [][2]int
		for _, l := range tm.re.FindAllStringIndex(text, -1) {
			want = append(want, [2]int{l[0], l[1]})
		}
		var sc scratch
		if got := m.find(tm, text, &sc); !reflect.DeepEqual(got, want) && (len(got) > 0 || len(want) > 0) {
			t.Fatalf("%q in %q (case=%v word=%v): got %v, regexp %v", q.Patterns[0], text, q.CaseSensitive, q.Word, got, want)
		}
	}
}

func TestSkippable(t *testing.T) {
	for s, want := range map[string]bool{
		"docker": true, "网络": true, "iptables-legacy": true, "localhost:5432": true,
		"USER:": false, "failed:": false, "tool": false, "toolbox": true, "redacted": false, "[img": false,
		"two words": false, "2026": false, `a"b`: false, "chars": false, "omitted]": false, "cancel": false, "unknown": false, "error": true, "rror": true, "error:": false, "user": false, "assistant": false, "aborted": true, "the": false, "subagents": true, "x=1": false,
	} {
		if got := skippable(s); got != want {
			t.Errorf("skippable(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestEarlyStop(t *testing.T) {
	e := earlyStop{limit: 2}
	hit := func(ts string) Hit { return Hit{Turn: Turn{TS: ts}} }
	e.add([]Hit{hit("2026-10-28T10:00:00Z"), hit("2026-10-28T09:00:00Z")})
	old := float64(time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC).UnixNano()) / 1e6
	if e.done(old) {
		t.Fatal("two hits cannot prove a limit of 2 is cut")
	}
	e.add([]Hit{hit("2026-10-28T08:00:00Z"), hit("2026-10-27T08:00:00Z")})
	if !e.done(old) {
		t.Fatalf("three hits newer than the file: %v", e.newest)
	}
	if e.done(old + 3*3600e3) {
		t.Fatal("a file newer than the third hit can still matter")
	}
	if (&earlyStop{}).done(0) {
		t.Fatal("no limit, no early stop")
	}
}

func TestScanOptionsKeepStructure(t *testing.T) {
	for _, f := range fixtureFiles(t) {
		full, err := sources.Open(f)
		if err != nil || full == nil {
			t.Fatal(err)
		}
		raw, err := sources.ReadRaw(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		want := SplitTurns(full.Lines(0))
		match := regexp.MustCompile(`test`)
		for _, keep := range []func([]byte, []ir.Role) bool{
			func([]byte, []ir.Role) bool { return false },
			func([]byte, []ir.Role) bool { return true },
			func(b []byte, _ []ir.Role) bool { return match.Match(b) },
		} {
			s, err := sources.OpenBytes(f, raw, sources.ScanOptions{Keep: keep})
			if err != nil {
				t.Fatal(err)
			}
			got := SplitTurns(s.Lines(0))
			if len(got) != len(want) {
				t.Fatalf("%s: %d turns, full read %d", f.Path, len(got), len(want))
			}
			for i := range got {
				if got[i].TS != want[i].TS || got[i].Lines[0] != want[i].Lines[0] {
					t.Fatalf("%s turn %d: %+v vs %+v", f.Path, i+1, got[i].Lines[0], want[i].Lines[0])
				}
				for _, l := range got[i].Lines {
					if l.Elided && (l.TurnStart || l.Text != "") {
						t.Fatalf("bad placeholder %+v", l)
					}
				}
			}
		}
		// Keeping every record and redacting afterwards gives exactly the full read.
		s, _ := sources.OpenBytes(f, raw, sources.ScanOptions{Keep: func([]byte, []ir.Role) bool { return true }, Raw: true})
		got := s.Lines(0)
		for i := range got {
			got[i] = redactTurn(Turn{Lines: got[i : i+1]}).Lines[0]
		}
		if !reflect.DeepEqual(got, full.Lines(0)) {
			t.Fatalf("%s: raw + redact differs from a full read", f.Path)
		}
	}
}

// spreadSessions writes n copies of the fixture session, the k-th moved k days into the past
// (timestamps and file mtime alike), like a history that grew over weeks.
func spreadSessions(t *testing.T, n int) []ir.SourceFile {
	t.Helper()
	src := filepath.Join(testutil.Sessions(t), testutil.MainRel)
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	base := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for k := 0; k < n; k++ {
		day := base.AddDate(0, 0, -k)
		data := strings.ReplaceAll(string(raw), "2026-10-28T", day.Format("2006-01-02")+"T")
		p := filepath.Join(dir, fmt.Sprintf("%s_%04d.jsonl", day.Format("2006-01-02"), k))
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		mtime := day.Add(5 * time.Hour) // after the session's last entry (~04:00)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	files, err := sources.FilesUnder(dir)
	if err != nil || len(files) != n {
		t.Fatalf("%d files: %v", len(files), err)
	}
	return files
}

func TestEarlyStopReturnsTheNewestHits(t *testing.T) {
	files := spreadSessions(t, 40)
	for _, q := range []Query{{Patterns: []string{"econnrefused"}, Limit: 3}, {Patterns: []string{"a"}, Limit: 1}, {Patterns: []string{"readme|test"}, Any: true, Limit: 10}} {
		want := referenceSearch(t, files, q)
		got, st, err := Search(files, q)
		if err != nil {
			t.Fatal(err)
		}
		if st.Complete || st.Skipped == 0 || st.Matched <= q.Limit {
			t.Fatalf("%q: expected an early stop with more than %d matches: %+v", q.Patterns, q.Limit, st)
		}
		sameHits(t, fmt.Sprintf("%q limit %d", q.Patterns, q.Limit), got, want[:q.Limit])
	}
	// Without a limit, everything is read.
	q := Query{Patterns: []string{"econnrefused"}}
	got, st, _ := Search(files, q)
	if !st.Complete || st.Skipped != 0 {
		t.Fatalf("no limit: %+v", st)
	}
	sameHits(t, "no limit", got, referenceSearch(t, files, q))
}

func TestTimeoutLeavesFilesUnread(t *testing.T) {
	files := spreadSessions(t, 10)
	q := Query{Patterns: []string{"econnrefused"}, Timeout: time.Nanosecond}
	hits, st, err := Search(files, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || !st.TimedOut || st.Complete || st.Unread != len(files) {
		t.Fatalf("expired before the first file: %d hits, %+v", len(hits), st)
	}
	// A timeout that does not expire changes nothing.
	q.Timeout = time.Hour
	got, st, _ := Search(files, q)
	q.Timeout = 0
	want, _, _ := Search(files, q)
	if st.TimedOut || !st.Complete || st.Unread != 0 {
		t.Fatalf("%+v", st)
	}
	sameHits(t, "generous timeout", got, want)
}

// Sessions inside a database go through ReadRaw (the stream mg prefilters) and OpenBytes.
func TestSearchEqualsReferenceOnDatabase(t *testing.T) {
	files, err := sources.FilesUnder(testutil.OpenCodeDB(t))
	if err != nil || len(files) != 2 {
		t.Fatalf("%+v %v", files, err)
	}
	checkEquivalence(t, files, append(equivalenceQueries(), Query{Patterns: []string{"econnrefused", "pnpm"}}, Query{Patterns: []string{"bash"}, Roles: []ir.Role{ir.RoleTool}}))
	hits, st, err := Search(files, Query{Patterns: []string{"ECONNREFUSED"}})
	if err != nil || len(hits) != 1 || hits[0].Session.Key == "" || hits[0].Session.ID != "ses_main" || st.MatchedSessions != 1 {
		t.Fatalf("%+v %+v %v", hits, st, err)
	}
	if !strings.HasSuffix(hits[0].Session.Path, "#ses_main") || hits[0].Ref() != "opencode:ses_main#1" {
		t.Fatalf("path %q ref %q", hits[0].Session.Path, hits[0].Ref())
	}
	// two sessions of one database are two sessions
	all, st, _ := Search(files, Query{Patterns: []string{"find|pnpm"}, Any: true})
	if len(all) != 2 || st.MatchedSessions != 2 {
		t.Fatalf("%d hits in %d sessions", len(all), st.MatchedSessions)
	}
}

// Sessions the agents themselves wrote (Gemini CLI, Qwen Code, opencode, omp): the optimised search
// must agree with the plain one on the text their renderers produce, notation included.
func TestSearchEqualsReferenceOnAgentFixtures(t *testing.T) {
	files, err := sources.FilesUnder(testutil.Fixture("."))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[ir.SourceKind]int{}
	for _, f := range files {
		kinds[f.Kind]++
	}
	for _, k := range []ir.SourceKind{ir.OMP, ir.Gemini, ir.Qwen, ir.OpenCode} {
		if kinds[k] == 0 {
			t.Fatalf("no %s sessions among %d files: %v", k, len(files), kinds)
		}
	}
	extra := []Query{
		{Patterns: []string{"exit"}}, {Patterns: []string{"recovered"}}, {Patterns: []string{"ECONNREFUSED"}},
		{Patterns: []string{"invoke_agent|task"}}, {Patterns: []string{"No such file"}}, {Patterns: []string{"failed"}},
		{Patterns: []string{"SCENARIO", "followup"}}, {Patterns: []string{"notes.txt"}, Roles: []ir.Role{ir.RoleResult}},
		{Patterns: []string{"subagent"}}, {Patterns: []string{"assistant"}}, {Patterns: []string{`exit \d`}, Regex: true},
		{Patterns: []string{"mem-grep-gen"}}, {Patterns: []string{"结束|完成"}}, {Patterns: []string{"run_shell_command", "bash"}, Any: true},
	}
	checkEquivalence(t, files, append(equivalenceQueries(), extra...))
	hits, _, err := Search(files, Query{Patterns: []string{"ECONNREFUSED"}, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[ir.SourceKind]bool{}
	for _, h := range hits {
		seen[h.Session.Agent] = true
	}
	for _, k := range []ir.SourceKind{ir.OMP, ir.Gemini, ir.Qwen, ir.OpenCode} {
		if !seen[k] {
			t.Errorf("ECONNREFUSED is in the %s fixture but was not found; found in %v", k, seen)
		}
	}
}
