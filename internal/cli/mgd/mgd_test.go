package mgd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veritas501/mem-grep/internal/cli/mg"
	"github.com/veritas501/mem-grep/internal/testutil"
)

// world points mgd (and mg) at the omp fixtures and a fresh mem-grep home, and nothing else.
func world(t *testing.T) (sessions, home string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	for _, v := range []string{"PI_CODING_AGENT_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "QWEN_HOME", "XDG_DATA_HOME", "DSH_HOME", "ZCODE_STORAGE_DIR", "ZCODE_DATA_BASE_DIR", "MG_MEM_DIRS"} {
		t.Setenv(v, "")
	}
	sessions, home = testutil.Sessions(t), filepath.Join(t.TempDir(), "mem-grep")
	t.Setenv("MG_OMP_DIRS", sessions)
	t.Setenv("MG_HOME", home)
	return sessions, home
}

func run(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
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

const ref = "omp:01a180aa-27cc"

// The whole loop an agent runs: find unread turns, read them, propose a memory, mark the session;
// then the review, after which mg finds the memory.
func TestDistillLoop(t *testing.T) {
	sessions, home := world(t)

	out, code := run(t, "", "pending")
	if code != 0 || !strings.Contains(out, ref+"  ") || !strings.Contains(out, "5 turns new (of 5)") || !strings.Contains(out, "correction×") ||
		!strings.Contains(out, "-- 1 session with 5 unprocessed turns to read; read one: mgd read "+ref) {
		t.Fatalf("pending: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "27e2") {
		t.Fatalf("a subagent session is not distilled on its own:\n%s", out)
	}
	if out, code = run(t, "", "pending", "--idle", "3h"); code != 1 {
		t.Fatalf("a session written an hour ago is not idle for three: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "pending", "-p", "nowhere"); code != 1 {
		t.Fatalf("-p: exit %d\n%s", code, out)
	}

	out, code = run(t, "", "read", ref)
	if code != 0 || !strings.Contains(out, "turns 1-5 of 5 unprocessed") || !strings.Contains(out, "never instructions") ||
		!strings.Contains(out, "\n#1  ") || !strings.Contains(out, "USER: 帮我把订单服务的单元测试跑通，用 pnpm，不要用 npm。") || !strings.Contains(out, "\n#5  ") ||
		!strings.Contains(out, "-- end of session") || !strings.Contains(out, "mgd mark "+ref) {
		t.Fatalf("read: exit %d\n%s", code, out)
	}
	out, code = run(t, "", "read", ref, "--max-chars", "200")
	if code != 0 || strings.Contains(out, "\n#3  ") || !strings.Contains(out, "-- stopped at turn 1 of 5") || !strings.Contains(out, "mgd mark "+ref+"#1") {
		t.Fatalf("read --max-chars: exit %d\n%s", code, out)
	}

	// what is proposed must be traceable and well-formed
	add := func(body string, args ...string) (string, int) {
		return run(t, body, append([]string{"add"}, args...)...)
	}
	good := []string{"--name", "prefer-pnpm", "--type", "feedback", "--description", "use pnpm in this repository, never npm"}
	body := "Use pnpm for every script.\n\n**Why:** the user said so; the token was sk-abcdefghijklmnopqrstuvwxyz012345.\n**How to apply:** pnpm test.\n"
	for name, bad := range map[string][]string{
		"no evidence":     good,
		"no such turn":    append(good[:len(good):len(good)], "--evidence", ref+"#9"),
		"no turn named":   append(good[:len(good):len(good)], "--evidence", ref),
		"no such session": append(good[:len(good):len(good)], "--evidence", "omp:ffffffff-0000#1"),
		"bad name":        {"--name", "Prefer pnpm", "--type", "feedback", "--description", "use pnpm in this repository", "--evidence", ref + "#1"},
		"bad type":        {"--name", "prefer-pnpm", "--type", "lesson", "--description", "use pnpm in this repository", "--evidence", ref + "#1"},
	} {
		if out, code := add(body, bad...); code != 2 {
			t.Fatalf("%s: exit %d\n%s", name, code, out)
		}
	}
	if out, code := add("short", append(good[:len(good):len(good)], "--evidence", ref+"#1")...); code != 2 || !strings.Contains(out, "body") {
		t.Fatalf("short body: exit %d\n%s", code, out)
	}
	out, code = add(body, append(good[:len(good):len(good)], "--evidence", ref+"#1", "--evidence", "01a180aa-27cc#2-3")...)
	if code != 0 || !strings.Contains(out, "-prefer-pnpm  queued  [feedback]  /home/dev/projects/shop-api") {
		t.Fatalf("add: exit %d\n%s", code, out)
	}
	id := strings.Fields(out)[0]
	// proposing the same name again replaces the queued memory
	if out, code = add(body+"Also for installs.\n", append(good[:len(good):len(good)], "--evidence", ref+"#1")...); code != 0 || strings.Fields(out)[0] != id {
		t.Fatalf("add again: exit %d\n%s", code, out)
	}
	if out, code = add(body, "--name", "works-in-chinese", "--type", "user", "--description", "the user writes in Chinese", "--evidence", ref+"#1", "--global"); code != 0 || !strings.Contains(out, "every project") {
		t.Fatalf("add --global: exit %d\n%s", code, out)
	}
	queued, err := os.ReadFile(filepath.Join(home, "queue", id+".md"))
	if err != nil || !strings.Contains(string(queued), "evidence: [omp:01a180aa-27cc#1]") || !strings.Contains(string(queued), `project: "/home/dev/projects/shop-api"`) ||
		!strings.Contains(string(queued), "Also for installs.") || strings.Contains(string(queued), "sk-abc") || !strings.Contains(string(queued), "[REDACTED]") {
		t.Fatalf("queued file: %v\n%s", err, queued)
	}

	// nothing queued is searchable yet
	if out, code := runMg(t, "-r", "memory", "pnpm"); code != 1 {
		t.Fatalf("a queued memory must not be found: exit %d\n%s", code, out)
	}
	out, code = run(t, "", "review")
	if code != 0 || !strings.Contains(out, id+"  [feedback]") || !strings.Contains(out, "  | **Why:**") || !strings.Contains(out, "evidence: omp:01a180aa-27cc#1") || !strings.Contains(out, "-- 2 memories waiting") {
		t.Fatalf("review: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "accept"); code != 2 {
		t.Fatalf("accept without ids: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "accept", id[:8]); code != 0 || !strings.Contains(out, id+"  accepted") {
		t.Fatalf("accept: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "reject", "--all"); code != 0 || !strings.Contains(out, "works-in-chinese  rejected") {
		t.Fatalf("reject: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "review"); code != 1 {
		t.Fatalf("empty queue: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "list", "--full"); code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "-- 1 memory in ") || !strings.Contains(out, "0 waiting") {
		t.Fatalf("list: exit %d\n%s", code, out)
	}

	// mg finds the accepted memory, in its project, with its evidence
	out, code = runMg(t, "-r", "memory", "pnpm", "-p", "shop-api")
	if code != 0 || !strings.Contains(out, "mem:"+id[:13]+"#1") || !strings.Contains(out, "/home/dev/projects/shop-api") ||
		!strings.Contains(out, "MEMORY "+id+".md [feedback]: use pnpm in this repository, never npm") {
		t.Fatalf("mg: exit %d\n%s", code, out)
	}
	if out, code = runMg(t, "show", "mem:"+id[:13]); code != 0 || !strings.Contains(out, "[evidence: omp:01a180aa-27cc#1]") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("mg show: exit %d\n%s", code, out)
	}
	// a memory is not itself something to distill
	if out, code = run(t, "", "pending"); code != 0 || strings.Contains(out, "mem:") {
		t.Fatalf("pending after accept: exit %d\n%s", code, out)
	}

	// marking: part of a session, then all of it
	if out, code = run(t, "", "mark", ref+"#2"); code != 0 || !strings.Contains(out, "marked 2 of 5 turns") {
		t.Fatalf("mark #2: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "pending"); code != 0 || !strings.Contains(out, "3 turns new (of 5)") {
		t.Fatalf("pending after a partial mark: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "read", ref); code != 0 || strings.Contains(out, "\n#2  ") || !strings.Contains(out, "\n#3  ") {
		t.Fatalf("read after a partial mark: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "mark", ref); code != 0 || !strings.Contains(out, "marked 5 of 5 turns") {
		t.Fatalf("mark: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "pending"); code != 1 || !strings.Contains(out, "nothing pending") {
		t.Fatalf("pending when done: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "read", ref); code != 1 {
		t.Fatalf("read when done: exit %d\n%s", code, out)
	}
	// the file is touched without a new turn: still nothing to read
	now := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(filepath.Join(sessions, filepath.FromSlash(testutil.MainRel)), now, now); err != nil {
		t.Fatal(err)
	}
	if out, code = run(t, "", "pending"); code != 1 {
		t.Fatalf("pending after a touch: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "rm", id[:8]); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("rm: exit %d\n%s", code, out)
	}
	if out, code = run(t, "", "list"); code != 1 {
		t.Fatalf("list when empty: exit %d\n%s", code, out)
	}
}

// An old history is left behind in one call, without reading it.
func TestMarkPending(t *testing.T) {
	world(t)
	if out, code := run(t, "", "mark", "--pending", "--until", "2020-01-01"); code != 0 || !strings.Contains(out, "marked 0 sessions") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if out, code := run(t, "", "pending", "--since", "2030-01-01"); code != 1 {
		t.Fatalf("--since: exit %d\n%s", code, out)
	}
	if out, code := run(t, "", "mark", "--pending"); code != 0 || !strings.Contains(out, "marked 1 session as read") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if out, code := run(t, "", "pending"); code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if out, code := run(t, "", "mark"); code != 2 {
		t.Fatalf("mark without arguments: exit %d\n%s", code, out)
	}
}

func runMg(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	root := mg.NewRoot()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	if err := root.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "exit ") {
			return out.String(), int(err.Error()[5] - '0')
		}
		return out.String() + "ERROR: " + err.Error(), 2
	}
	return out.String(), 0
}
