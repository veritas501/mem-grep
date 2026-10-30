package grep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/sources"
	"github.com/veritas501/mem-grep/internal/testutil"
)

func fixtureFiles(t *testing.T) []ir.SourceFile {
	t.Helper()
	files, err := sources.FilesUnder(testutil.Sessions(t))
	if err != nil || len(files) != 2 {
		t.Fatalf("%v %v", files, err)
	}
	return files
}

func search(t *testing.T, q Query) []Hit {
	t.Helper()
	hits, _, err := Search(fixtureFiles(t), q)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestTermsMustMeetInOneTurn(t *testing.T) {
	// "ECONNREFUSED" and "mock" occur in the same turn (the failed run and the attempt to mock).
	hits := search(t, Query{Patterns: []string{"econnrefused", "mock"}})
	if len(hits) != 1 || hits[0].Turn.Index != 1 || hits[0].Session.Agent != ir.OMP {
		t.Fatalf("%+v", hits)
	}
	// "ECONNREFUSED" and "/clear" never share a turn
	if hits := search(t, Query{Patterns: []string{"econnrefused", "README"}}); len(hits) != 0 {
		t.Fatalf("AND across turns: %+v", hits)
	}
	if hits := search(t, Query{Patterns: []string{"econnrefused", "README"}, Any: true}); len(hits) < 2 {
		t.Fatalf("--any: %d", len(hits))
	}
}

func TestAlternativesAndSmartCase(t *testing.T) {
	if hits := search(t, Query{Patterns: []string{"nosuchword|test-db"}}); len(hits) == 0 {
		t.Fatal("alternatives")
	}
	if hits := search(t, Query{Patterns: []string{"Econnrefused"}}); len(hits) == 0 {
		t.Fatal("case-insensitive by default")
	}
	if hits := search(t, Query{Patterns: []string{"Econnrefused"}, CaseSensitive: true}); len(hits) != 0 {
		t.Fatal("case-sensitive on request")
	}
	if hits := search(t, Query{Patterns: []string{"数据库"}}); len(hits) == 0 {
		t.Fatal("Chinese")
	}
}

func TestRoleFilterAndRegex(t *testing.T) {
	user := search(t, Query{Patterns: []string{"pnpm"}, Roles: []ir.Role{ir.RoleUser}})
	tool := search(t, Query{Patterns: []string{"pnpm"}, Roles: []ir.Role{ir.RoleTool}})
	if len(user) != 1 || len(tool) == 0 {
		t.Fatalf("user=%d tool=%d", len(user), len(tool))
	}
	re := search(t, Query{Patterns: []string{`docker compose -f \S+`}, Regex: true})
	if len(re) == 0 {
		t.Fatal("regex")
	}
	top := Top(re, true)
	if top[0].Match != "docker compose -f docker-compose.test.yml" || top[0].Example == "" {
		t.Fatalf("%+v", top)
	}
}

func TestFiltersAndPrefilter(t *testing.T) {
	files := fixtureFiles(t)
	if hits, _, _ := Search(files, Query{Patterns: []string{"pnpm"}, Project: "SHOP-API"}); len(hits) == 0 {
		t.Fatal("project filter is case-insensitive substring")
	}
	if hits, _, _ := Search(files, Query{Patterns: []string{"pnpm"}, Project: "other"}); len(hits) != 0 {
		t.Fatal("project filter")
	}
	if hits, _, _ := Search(files, Query{Patterns: []string{"pnpm"}, Agents: []ir.SourceKind{ir.Codex}}); len(hits) != 0 {
		t.Fatal("agent filter")
	}
	_, st, _ := Search(files, Query{Patterns: []string{"zzz-not-there"}})
	if st.Skipped != 2 || st.Parsed != 0 {
		t.Fatalf("prefilter should skip both files: %+v", st)
	}
	// secrets are redacted in the transcript, so they are not found either
	if hits, _, _ := Search(files, Query{Patterns: []string{"sk-ant-api03"}}); len(hits) != 0 {
		t.Fatal("secret found")
	}
	future := time.Now().Add(time.Hour)
	if hits, _, _ := Search(files, Query{Patterns: []string{"pnpm"}, Since: future}); len(hits) != 0 {
		t.Fatal("since")
	}
}

func TestResolveRefs(t *testing.T) {
	files := fixtureFiles(t)
	r, err := ParseRef("omp:01a180aa-27cc#2-3")
	if err != nil || r.Agent != ir.OMP || r.From != 2 || r.To != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	s, err := Resolve(files, r)
	if err != nil || s.Cwd != testutil.CWD {
		t.Fatalf("%v", err)
	}
	if _, err := Resolve(files, Ref{ID: "01a180aa"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("both fixtures share the prefix: %v", err)
	}
	p := filepath.Join(filepath.Dir(files[0].Path), "main.jsonl")
	if _, err := os.Stat(p); err == nil {
		if r, _ := ParseRef(p + "#1"); !r.IsPath || r.From != 1 {
			t.Fatalf("%+v", r)
		}
	}
}

func TestListSessions(t *testing.T) {
	ls, err := List(fixtureFiles(t), Query{})
	if err != nil || len(ls) != 2 {
		t.Fatalf("%v %v", ls, err)
	}
	for _, l := range ls {
		if l.First == "" || l.Turns == 0 {
			t.Fatalf("%+v", l)
		}
	}
}

func TestSnippet(t *testing.T) {
	text := strings.Repeat("a", 100) + "NEEDLE" + strings.Repeat("b", 100)
	s, r := Snippet(text, [][2]int{{100, 106}}, 30)
	if !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || s[r[0][0]:r[0][1]] != "NEEDLE" {
		t.Fatalf("%q %v", s, r)
	}
	s, r = Snippet("line one\n    line 二 x", [][2]int{{22, 23}}, 0)
	if s != "line one ⏎ line 二 x" || s[r[0][0]:r[0][1]] != "x" {
		t.Fatalf("%q %v", s, r)
	}
}

func TestParseTimeBound(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d": now.AddDate(0, 0, -7), "36h": now.Add(-36 * time.Hour), "2w": now.AddDate(0, 0, -14), "1m": now.AddDate(0, -1, 0),
	} {
		if got, err := ParseTimeBound(in, now); err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := ParseTimeBound("2026-10-01", now); err != nil {
		t.Error(err)
	}
	if _, err := ParseTimeBound("soon", now); err == nil {
		t.Error("should fail")
	}
}

func TestRegexLiteralPrefixPrefilters(t *testing.T) {
	_, st, _ := Search(fixtureFiles(t), Query{Patterns: []string{`kubectl\s+\w+`}, Regex: true})
	if st.Skipped != 2 {
		t.Fatalf("files without the literal prefix should be skipped: %+v", st)
	}
	if hits, _, _ := Search(fixtureFiles(t), Query{Patterns: []string{`docker compose -f \S+`}, Regex: true}); len(hits) == 0 {
		t.Fatal("prefiltered regex still matches")
	}
}

func TestContainsFoldASCII(t *testing.T) {
	for _, c := range []struct {
		hay, needle string
		want        bool
	}{{"Docker NETWORK down", "network", true}, {"ab", "abc", false}, {"测试 Docker", "测试 docker", true}, {"xxDoCkEr", "docker", true}, {"dock", "docker", false}} {
		if got := containsFoldASCII([]byte(c.hay), []byte(c.needle)); got != c.want {
			t.Errorf("%q in %q = %v", c.needle, c.hay, got)
		}
	}
	if asciiFoldable([]byte("é")) || !asciiFoldable([]byte("网络 docker")) {
		t.Error("asciiFoldable")
	}
}
