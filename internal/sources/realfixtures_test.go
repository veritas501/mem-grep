package sources

import (
	"regexp"
	"strings"
	"testing"

	"github.com/veritas501/mem-grep/internal/ir"
)

// Sessions the agents wrote while talking to a real model (DeepSeek, see test/fixtures/real/gen.sh):
// a small Go module with a bug, two prompts — fix it, then add a function — in one resumed session.
// The words differ on every regeneration, so these tests only assert what must hold for any
// conversation: the structure, every tool call paired with its own result, no agent-side wrappers.

var (
	callRe   = regexp.MustCompile(`^ {2}→ (\S+?)\(`)
	resultRe = regexp.MustCompile(`^ {2}[✓✗] (\S+?)(?::| failed:)`)
)

func TestSessionsWrittenWithARealModel(t *testing.T) {
	for _, c := range []struct {
		kind       ir.SourceKind
		rel        string
		failsFirst bool // the shell tool reports the failing `go test` as a failure
	}{
		{ir.Qwen, "real/qwen/projects", true},
		{ir.Gemini, "real/gemini/tmp", false}, // the model piped the output through `tail`, which hides the exit code
		{ir.OpenCode, "real/opencode/opencode.db", true},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			sessions := renderAll(t, c.kind, c.rel)
			if len(sessions) != 1 {
				t.Fatalf("want one session, got %v", keys(sessions))
			}
			for _, r := range sessions {
				first := r.lines[0]
				if first.Role != ir.RoleUser || !first.TurnStart || !strings.Contains(first.Text, "go test") {
					t.Fatalf("first line: %+v", first)
				}
				turns, failed := 0, 0
				calls, results := map[string]int{}, map[string]int{}
				for _, l := range r.lines {
					if l.TurnStart {
						turns++
					}
					if l.Success != nil && !*l.Success {
						failed++
					}
					for _, line := range strings.Split(l.Text, "\n")[:1] {
						if m := callRe.FindStringSubmatch(line); m != nil {
							calls[m[1]]++
						}
						if m := resultRe.FindStringSubmatch(line); m != nil {
							results[m[1]]++
						}
					}
				}
				if turns != 2 {
					t.Errorf("two prompts make two turns, got %d", turns)
				}
				if len(calls) == 0 {
					t.Fatalf("no tool calls rendered:\n%s", r.text)
				}
				for name, n := range calls {
					if results[name] != n {
						t.Errorf("%s: %d calls but %d results\n%s", name, n, results[name], r.text)
					}
				}
				if c.failsFirst && failed == 0 {
					t.Errorf("the failing test run must show up as a failed result:\n%s", r.text)
				}
				assertNoNoise(t, string(c.kind), r.text, "<untrusted_context>", "<session_context>", "Process Group PGID", "Exit Code:", "Command: ", "Directory: (root)", "<system-reminder>")
				if r.sess.Cwd != "/tmp/mem-grep-gen/real-work" {
					t.Errorf("cwd %q", r.sess.Cwd)
				}
				// the second prompt asks for Sub: resuming from the second turn renders only that turn
				second := int64(-1)
				n := 0
				for _, l := range r.lines {
					if l.TurnStart {
						if n++; n == 2 {
							second = l.Start
						}
					}
				}
				later := ir.Text(r.sess.Render(second, -1))
				if !strings.HasPrefix(later, "USER: ") || !strings.Contains(later, "Sub") || strings.Contains(later, "Add(a, b int)") && strings.Contains(later, "FAIL") {
					t.Errorf("second turn:\n%s", later)
				}
				for _, l := range r.lines {
					if l.Start < 0 || l.End <= l.Start || l.End > r.sess.CompleteBytes {
						t.Fatalf("offsets of %q: [%d,%d) in %d", l.Text, l.Start, l.End, r.sess.CompleteBytes)
					}
				}
			}
		})
	}
}
