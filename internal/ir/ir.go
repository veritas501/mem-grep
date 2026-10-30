// Package ir is the agent-neutral intermediate representation.
//
// Every source adapter (omp, Claude Code, Codex, Gemini CLI, Qwen Code, opencode) turns its own
// log format into these types. Everything downstream — search, turn grouping, session listing,
// `show` and JSON output — consumes only the IR and never looks at source records. When adding an
// agent, the rule of thumb is: if a downstream package needs to know which agent wrote a session,
// the information belongs in the IR instead.
package ir

import (
	"strings"

	"github.com/veritas501/mem-grep/internal/util"
)

// SourceKind names the agent that wrote a session.
type SourceKind string

const (
	OMP      SourceKind = "omp"
	Claude   SourceKind = "claude"
	Codex    SourceKind = "codex"
	Gemini   SourceKind = "gemini"
	Qwen     SourceKind = "qwen"
	OpenCode SourceKind = "opencode"
)

// Role of a transcript line.
type Role string

const (
	RoleUser      Role = "user"
	RoleTask      Role = "task"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleResult    Role = "result"
	RoleShell     Role = "shell"
	RoleNote      Role = "note"
)

// Line is one rendered line of a compact transcript. Text is already redacted and uses the
// shared notation the renderers use ("USER: …", "  → tool(args)", "  ✗ tool failed: …").
type Line struct {
	// EntryID is the id of the source record this line came from; stable across runs (used in refs
	// and incremental reads).
	EntryID string
	TS      string
	Role    Role
	Text    string
	// TurnStart marks lines that start a new turn (user message, subagent task, branch/reset).
	TurnStart bool
	// Start and End are the byte range of the source record in the (decompressed) file, so a
	// reader can resume from End without knowing the format.
	Start, End int64
	// Success is the structured outcome of a tool result when the source records one (omp
	// isError, Claude is_error); nil when only text is available.
	// TODO: nothing reads this yet. Intended for deterministic signals such as "failed, then
	// succeeded with the same call", which would mark a lesson as verified (idea from memantine).
	Success *bool
	// Elided marks a placeholder for a record a search skipped without decoding (see
	// sources.ScanOptions.Keep): it has no Text, never starts a turn, and stands for zero or
	// more lines a full read would render. Only searches see such lines.
	Elided bool
}

// Bool returns a pointer to b, for Line.Success.
func Bool(b bool) *bool { return &b }

// Text joins the lines' text.
func Text(lines []Line) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l.Text)
	}
	return b.String()
}

// Turn is one user message plus everything the agent did in response.
type Turn struct {
	FirstEntry, LastEntry string
	TS                    string
	Text                  string
	// Start and End are the byte range covered by the turn's lines.
	Start, End int64
}

// GroupTurns splits lines into turns at TurnStart lines; each turn's text is cut to maxChars.
func GroupTurns(lines []Line, maxChars int) []Turn {
	var turns []Turn
	var cur []Line
	flush := func() {
		if len(cur) == 0 {
			return
		}
		turns = append(turns, Turn{
			FirstEntry: cur[0].EntryID,
			LastEntry:  cur[len(cur)-1].EntryID,
			TS:         cur[0].TS,
			Text:       util.TruncateMiddle(Text(cur), maxChars),
			Start:      cur[0].Start,
			End:        cur[len(cur)-1].End,
		})
		cur = nil
	}
	for _, l := range lines {
		if l.TurnStart && len(cur) > 0 {
			flush()
		}
		cur = append(cur, l)
	}
	flush()
	return turns
}

// SourceFile is a session artifact found on disk by an adapter.
type SourceFile struct {
	Kind SourceKind
	// Path is the physical file to read.
	Path string
	// Key identifies the session across representations. It equals Path except where an agent
	// moves a session between representations: Codex
	// compresses cold rollouts to .jsonl.zst and decompresses them on resume, so its key is the
	// plain .jsonl path and stays the same either way.
	Key     string
	Size    int64
	MtimeMs float64
	// Inner names the session inside a Path that holds many (an opencode SQLite database); empty
	// for one-session files. Such a session has no byte size of its own: its Size is 0 and the
	// adapter reports the stream size in Session.CompleteBytes once it has opened it.
	Inner string
	// ParentKey is set for subagent / advisor files: the key of the parent session.
	ParentKey string
	AgentName string
	IsAdvisor bool
}

// Session is a parsed session, ready to render.
type Session struct {
	Kind      SourceKind
	File      SourceFile
	SessionID string
	// Cwd is the working directory the session ran in — the project a memory belongs to.
	Cwd       string
	Title     string
	StartedAt string
	// CompleteBytes counts the bytes that form complete, parsed records; anything beyond is a
	// partial write in progress.
	CompleteBytes int64
	BadLines      int
	// SkipReason is set when the session is not the user's conversation and must not be searched,
	// e.g. sessions an agent runs for itself (Codex memory consolidation, non-interactive exec).
	SkipReason string
	// Render returns lines for records starting in [from, to). Implementations may walk earlier
	// records too (omp does, to keep branch detection right on incremental reads). to < 0 means
	// "to the end".
	Render func(from, to int64) []Line
}

// Lines renders the whole session from offset `from`.
func (s *Session) Lines(from int64) []Line { return s.Render(from, -1) }
