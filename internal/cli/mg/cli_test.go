package mg

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/testutil"
)

// mg runs the command line on a copy of the fixtures and returns stdout and the exit status.
func mg(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	root := NewRoot()
	root.SetArgs(append([]string{"--path", testutil.Sessions(t)}, args...))
	root.SetOut(&out)
	root.SetErr(&errb)
	err := root.Execute()
	var code exitCode
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &code):
		return out.String(), int(code)
	}
	t.Logf("stderr: %s", errb.String())
	return out.String() + "ERROR: " + err.Error(), 2
}

func TestSearchOutputIsCompactWithRefsAndFooter(t *testing.T) {
	out, code := mg(t, "econnrefused", "mock")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "omp:01a180aa-27cc#1  ") || !strings.HasSuffix(lines[0], "(turn 1/5)") {
		t.Fatalf("header: %q", lines[0])
	}
	if lines[1] != "  USER: 帮我把订单服务的单元测试跑通，用 pnpm，不要用 npm。" {
		t.Fatalf("anchor: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "> ✗ bash failed: Error: connect ECONNREFUSED") {
		t.Fatalf("hit: %q", lines[2])
	}
	if !strings.Contains(out, "-- read it in full with the turn before and two after: mg show omp:01a180aa-27cc#1 -B 1 -A 2") {
		t.Fatal(out)
	}
}

func TestNoMatchExitsOneWithHints(t *testing.T) {
	out, code := mg(t, "kubernetes")
	if code != 1 || !strings.Contains(out, "-- no matches") || !strings.Contains(out, "--any") {
		t.Fatalf("%d %q", code, out)
	}
	if _, code := mg(t, "-r", "nobody", "x"); code != 2 {
		t.Fatalf("bad flag value must exit 2, got %d", code)
	}
}

func TestJSONLinesAreParseable(t *testing.T) {
	out, code := mg(t, "--json", "pnpm")
	if code != 0 {
		t.Fatal(out)
	}
	var types []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("not JSON: %q", sc.Text())
		}
		types = append(types, v["type"].(string))
		if v["type"] == "hit" {
			for _, l := range v["lines"].([]any) {
				line := l.(map[string]any)
				ms, _ := line["match"].([]any) // omitted on lines without a match
				for _, m := range ms {
					r := m.([]any)
					text := line["text"].(string)
					if got := strings.ToLower(text[int(r[0].(float64)):int(r[1].(float64))]); got != "pnpm" {
						t.Fatalf("offsets point at %q", got)
					}
				}
			}
		}
	}
	if types[len(types)-1] != "summary" || types[0] != "hit" {
		t.Fatal(types)
	}
}

func TestShowExpandsFollowingTurns(t *testing.T) {
	out, code := mg(t, "show", "omp:01a180aa-27cc#2", "-A", "1")
	if code != 0 || !strings.Contains(out, "\n#2  ") || !strings.Contains(out, "\n#3  ") || strings.Contains(out, "\n#4  ") {
		t.Fatalf("%d %s", code, out)
	}
	if !strings.Contains(out, "-- turns 2-3 of 5; next: mg show omp:01a180aa-27cc#4-5") {
		t.Fatal(out)
	}
	if _, code := mg(t, "show", "omp:01a180aa"); code != 2 {
		t.Fatal("an ambiguous prefix must be an error")
	}
}

func TestTopAndSessions(t *testing.T) {
	out, code := mg(t, "-e", `docker compose -f \S+`, "--top")
	if code != 0 || !strings.Contains(out, "docker compose -f docker-compose.test.yml") || !strings.Contains(out, "omp:01a180aa-27cc#") {
		t.Fatalf("%d %s", code, out)
	}
	out, _ = mg(t, "sessions")
	if strings.Count(out, "\nomp:")+strings.Count(out[:4], "omp:") != 2 || !strings.Contains(out, `subagent "scout"`) {
		t.Fatal(out)
	}
	out, _ = mg(t, "sessions", "pnpm")
	if !strings.Contains(out, "2 hits") || strings.Contains(out, "scout") {
		t.Fatal(out)
	}
}

func TestRoleFilter(t *testing.T) {
	out, _ := mg(t, "-r", "user", "pnpm")
	if !strings.Contains(out, "1 turn in 1 session") {
		t.Fatal(out)
	}
	out, _ = mg(t, "-r", "tool", "-r", "result", "pnpm")
	if strings.Contains(out, "> USER:") {
		t.Fatal(out)
	}
}

func TestRoleFilterAlsoLimitsWhatIsPrinted(t *testing.T) {
	// --full prints the whole turn, but -r user keeps only the user's lines (plus the anchor).
	out, code := mg(t, "--full", "-r", "user", "pnpm")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "> ") {
			if body := strings.TrimSpace(l[2:]); !strings.HasPrefix(body, "USER:") {
				t.Fatalf("a line of another role was printed: %q\n%s", l, out)
			}
		}
	}
	// show takes -r too, and says what it left out.
	out, code = mg(t, "show", "omp:01a180aa-27cc#1", "-r", "user")
	if code != 0 || !strings.Contains(out, "USER: 帮我把订单服务") || strings.Contains(out, "ASSISTANT:") || !strings.Contains(out, "of other roles not shown]") {
		t.Fatalf("show -r user: %d\n%s", code, out)
	}
	if _, code := mg(t, "show", "omp:01a180aa-27cc#1", "-r", "nobody"); code != 2 {
		t.Fatalf("bad role must exit 2, got %d", code)
	}
}

func TestShowAcceptsFull(t *testing.T) {
	show := func(args ...string) string {
		out, code := mg(t, append([]string{"show", "omp:01a180aa-27cc#1"}, args...)...)
		if code != 0 {
			t.Fatalf("show %v: exit %d\n%s", args, code, out)
		}
		var keep []string
		for _, l := range strings.Split(out, "\n") {
			if !strings.HasPrefix(l, "file: ") { // a fresh fixture copy each call
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	plain := show()
	if show("--full") != plain {
		t.Fatal("show --full must be accepted and print the same uncut lines")
	}
	if show("--width", "20") == plain || show("--width", "20", "--full") != plain {
		t.Fatal("--full must win over --width")
	}
}

func TestSummaryReportsCompleteness(t *testing.T) {
	out, _ := mg(t, "--json", "pnpm")
	sc := bufio.NewScanner(strings.NewReader(out))
	var last map[string]any
	for sc.Scan() {
		_ = json.Unmarshal(sc.Bytes(), &last)
	}
	if last["type"] != "summary" || last["complete"] != true {
		t.Fatalf("summary: %v", last)
	}
}

func TestTimeoutIsInconclusiveNotNoMatch(t *testing.T) {
	// A search that ran out of time found nothing in what it read: exit 3, never 1.
	out, code := mg(t, "--timeout", "1ns", "pnpm")
	if code != 3 || !strings.Contains(out, "inconclusive") || !strings.Contains(out, "TIMED OUT after 1ns") || !strings.Contains(out, "does not mean nothing matches") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	jout, jcode := mg(t, "--json", "--timeout", "1ns", "pnpm")
	var last map[string]any
	for _, l := range strings.Split(strings.TrimSpace(jout), "\n") {
		_ = json.Unmarshal([]byte(l), &last)
	}
	if jcode != 3 || last["type"] != "summary" || last["timed_out"] != true || last["complete"] != false {
		t.Fatalf("exit %d summary %v", jcode, last)
	}
	// With time to spare nothing changes.
	out, code = mg(t, "--timeout", "1h", "pnpm")
	if code != 0 || strings.Contains(out, "TIMED OUT") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestMaxCharsStopsPrintingButKeepsTheFirstHit(t *testing.T) {
	all, _ := mg(t, "a")
	heads := func(s string) (n int) {
		for _, l := range strings.Split(s, "\n") {
			if strings.HasPrefix(l, "omp:") {
				n++
			}
		}
		return
	}
	if heads(all) < 3 {
		t.Fatalf("the query should match several turns:\n%s", all)
	}
	out, code := mg(t, "--max-chars", "10", "a")
	if code != 0 || heads(out) != 1 || !strings.Contains(out, "-- output budget of 10 characters reached: showing 1;") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	jout, _ := mg(t, "--json", "--max-chars", "10", "a")
	var last map[string]any
	for _, l := range strings.Split(strings.TrimSpace(jout), "\n") {
		_ = json.Unmarshal([]byte(l), &last)
	}
	if last["budget_exhausted"] != true || last["truncated"] != true || last["shown"] != float64(1) {
		t.Fatalf("summary %v", last)
	}
	// show stops between turns and says where to go on
	out, code = mg(t, "show", "omp:01a180aa-27cc", "--max-chars", "50")
	if code != 0 || !strings.Contains(out, "-- turns 1-1 of 5; output budget of 50 characters reached; next: mg show omp:01a180aa-27cc#2-") {
		t.Fatalf("show: exit %d\n%s", code, out)
	}
	jout, _ = mg(t, "show", "omp:01a180aa-27cc", "--json", "--max-chars", "50")
	last = nil
	for _, l := range strings.Split(strings.TrimSpace(jout), "\n") {
		_ = json.Unmarshal([]byte(l), &last)
	}
	if last["type"] != "summary" || last["to"] != float64(1) || last["budget_exhausted"] != true || last["next"] == nil {
		t.Fatalf("show summary %v", last)
	}
}

func TestSearchesADatabaseOfSessions(t *testing.T) {
	db := testutil.OpenCodeDB(t)
	run := func(args ...string) (string, int) {
		var out, errb bytes.Buffer
		root := NewRoot()
		root.SetArgs(append([]string{"--path", db}, args...))
		root.SetOut(&out)
		root.SetErr(&errb)
		err := root.Execute()
		var code exitCode
		if errors.As(err, &code) {
			return out.String(), int(code)
		}
		if err != nil {
			return out.String() + "ERROR: " + err.Error(), 2
		}
		return out.String(), 0
	}
	out, code := run("econnrefused")
	if code != 0 || !strings.Contains(out, "opencode:ses_main#1  ") || !strings.Contains(out, "/home/dev/shop") || !strings.Contains(out, "> ✗ bash failed: connect ECONNREFUSED") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	out, code = run("show", "opencode:ses_main#1", "-r", "user")
	if code != 0 || !strings.Contains(out, "file: "+db+"#ses_main") || !strings.Contains(out, "USER: 用 pnpm 跑测试") {
		t.Fatalf("show: exit %d\n%s", code, out)
	}
	out, code = run("sessions")
	if code != 0 || !strings.Contains(out, "opencode:ses_main") || !strings.Contains(out, "opencode:ses_sub") {
		t.Fatalf("sessions: exit %d\n%s", code, out)
	}
	if out, code = run("-a", "opencode", "find"); code != 0 || !strings.Contains(out, "opencode:ses_sub#1") || !strings.Contains(out, "subagent:") {
		t.Fatalf("subagent: exit %d\n%s", code, out)
	}
}

// Without --path, mg looks where the agents keep their sessions; MG_<AGENT>_DIRS moves that, and
// an invalid switch is an error, not a silent default.
func TestSessionRootsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // nothing at the default locations
	t.Setenv("USERPROFILE", t.TempDir())
	for _, v := range []string{"PI_CODING_AGENT_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "QWEN_HOME", "XDG_DATA_HOME"} {
		t.Setenv(v, "")
	}
	run := func(args ...string) (string, int) {
		var out, errb bytes.Buffer
		root := NewRoot()
		root.SetArgs(args)
		root.SetOut(&out)
		root.SetErr(&errb)
		err := root.Execute()
		var code exitCode
		switch {
		case err == nil:
			return out.String(), 0
		case errors.As(err, &code):
			return out.String(), int(code)
		}
		return out.String() + "ERROR: " + err.Error(), 2
	}
	if out, code := run("pnpm"); code != 1 {
		t.Fatalf("no sessions anywhere: exit %d\n%s", code, out)
	}
	t.Setenv("MG_OMP_DIRS", testutil.Sessions(t))
	if out, code := run("pnpm"); code != 0 || !strings.Contains(out, "omp:01a180aa") {
		t.Fatalf("MG_OMP_DIRS: exit %d\n%s", code, out)
	}
	t.Setenv("MG_SUBAGENTS", "maybe")
	if out, code := run("pnpm"); code != 2 || !strings.Contains(out, "MG_SUBAGENTS") {
		t.Fatalf("invalid switch: exit %d\n%s", code, out)
	}
}
