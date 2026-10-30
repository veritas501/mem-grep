package sources

// Claude Code adapter — searched like every other agent. Wrapper records and the subagent layout
// were checked against real 2.x transcripts; there is still no fixture.
//
// What is known. Claude Code is closed source; this comes from the record structure of real
// Claude Code 2.x transcripts, inspected on 2026-10-28 and 2026-10-30 (keys and counts only):
//   - Files: ~/.claude/projects/<encoded cwd>/<sessionId>.jsonl. There is no header line; every
//     record carries sessionId, cwd, gitBranch, version and timestamp.
//   - Records form a tree through uuid / parentUuid. `last-prompt` records carry `leafUuid`.
//   - "user" and "assistant" records carry an Anthropic Messages API `message`. One API response
//     is split into several assistant records, one per content block (text / thinking /
//     tool_use), which share message.id and differ in apiBlockIndex.
//   - Tool results arrive as "user" records whose content holds tool_result blocks (the record
//     also has a structured `toolUseResult`). They are not the user speaking.
//   - Local slash commands are "user" records whose text is wrapper tags rather than what the
//     user typed: <command-name>…</command-name> plus <command-message> and <command-args>. The
//     command's output is a second "user" record wrapped in <local-command-stdout>. A user
//     interrupt is a plain "user" record "[Request interrupted by user]" (or "… for tool use]").
//     These render as notes, not as USER lines.
//   - Subagent (sidechain) records carry isSidechain; each sidechain transcript is a separate
//     file at <encoded cwd>/<sessionId>/subagents/agent-<agentId>.jsonl (with a sibling
//     agent-*.meta.json). Sidechain records inside a main transcript were not seen.
//   - Text that is injected rather than typed: "user" records with isMeta, "attachment" records
//     (system reminders, hook context) and "system" records.
//   - Bookkeeping to ignore: mode, cost-state, tools-used, queue-operation, last-prompt, atis-latch.
//   - Claude Code deletes transcripts older than `cleanupPeriodDays`, so older sessions cannot be
//     searched.
//
// Before enabling it by default:
//   TODO(fixture): generate test/fixtures/claude/ with `claude -p` in a temporary HOME. Never copy
//   a real session: they contain secrets. Then add tests mirroring the omp ones (rendering,
//   incremental offsets, a partially written last line, subagents).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// Claude is the Claude Code adapter.
type Claude struct{}

func (Claude) Kind() ir.SourceKind { return ir.Claude }

func (Claude) Discover(cfg *config.Config) []ir.SourceFile {
	var out []ir.SourceFile
	for _, root := range cfg.Dirs(ir.Claude) {
		root = filepath.Clean(root)
		for _, f := range walkFiles(root, func(n string) bool { return strings.HasSuffix(n, ".jsonl") }, 3) {
			// Main transcripts sit directly in the project directory: <root>/<encoded cwd>/<id>.jsonl.
			// Subagent transcripts sit under a directory named after the parent session:
			// <root>/<enc>/<sessionId>/subagents/agent-*.jsonl (verified on real transcripts).
			// The directory layout alone does not identify Claude transcripts (omp uses the same
			// shape), so check the content too.
			if head, err := readHead(f.path, 16384); err != nil || !(Claude{}).Sniff(f.path, head) {
				continue
			}
			rel, _ := filepath.Rel(root, f.path)
			sf := ir.SourceFile{Kind: ir.Claude, Path: f.path, Key: f.path, Size: f.size, MtimeMs: f.mtimeMs}
			if strings.Count(rel, string(filepath.Separator)) > 1 {
				parent := ""
				for d := filepath.Dir(f.path); len(d) > len(root); d = filepath.Dir(d) {
					if _, err := os.Stat(d + ".jsonl"); err == nil {
						parent = d + ".jsonl"
						break
					}
				}
				if parent == "" || !cfg.Subagents() {
					continue // unknown nested file, or subagents disabled
				}
				sf.ParentKey = parent
				sf.AgentName = strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
			}
			out = append(out, sf)
		}
	}
	return out
}

func (Claude) Sniff(_ string, head []byte) bool {
	for i, l := range strings.Split(string(head), "\n") {
		if i >= 20 {
			break
		}
		var o map[string]any
		if json.Unmarshal([]byte(l), &o) != nil {
			continue
		}
		if _, ok := o["payload"]; ok {
			return false // Codex
		}
		if qwenShaped([]byte(l)) {
			return false // Qwen Code: same uuid / sessionId / cwd fields, but GenAI `parts`
		}
		if str(o, "sessionId") != "" {
			_, hasUUID := o["uuid"]
			_, hasLeaf := o["leafUuid"]
			_, hasMode := o["mode"]
			if hasUUID || hasLeaf || hasMode {
				return true
			}
		}
	}
	return false
}

var bashLikeRe = regexp.MustCompile(`(?i)bash|shell`)

var claudeInterruptRe = regexp.MustCompile(`^\[Request interrupted by user(?: for tool use)?\]$`)

// claudeUserNote turns a user record that is not the user speaking into the note mg renders,
// and reports whether record text starts a new turn. ok is false for ordinary typed text.
//
// Claude Code writes a local slash command as wrapper tags instead of text, and the command's
// output as a second user record (seen in real 2.x transcripts):
//
//	<command-name>/model</command-name>
//	<command-message>model</command-message>
//	<command-args>jd/claude-opus-4.8</command-args>
//	<local-command-stdout>Set model to Opus 4 ...</local-command-stdout>
//
// A user interrupt is a plain-text user record, "[Request interrupted by user]" or
// "[Request interrupted by user for tool use]".
func claudeUserNote(text string) (line string, turnStart, ok bool) {
	if cmd, rest, isCmd := claudeTag(text, "command-name"); isCmd {
		args := ""
		if i := strings.Index(rest, "<command-args>"); i >= 0 {
			if j := strings.Index(rest[i:], "</command-args>"); j >= 0 {
				args = rest[i+len("<command-args>") : i+j]
			}
		}
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			return "", false, true // an empty wrapper is still not user text
		}
		if args = strings.TrimSpace(args); args != "" {
			cmd += " " + args
		}
		if cmd == "/clear" {
			return "[user ran /clear — a new topic likely starts here]", true, true
		}
		return "[user ran " + util.TruncateMiddle(cmd, 300) + "]", false, true
	}
	if out, _, isOut := claudeTag(text, "local-command-stdout"); isOut {
		out = strings.TrimSpace(spaceRe.ReplaceAllString(out, " "))
		if out == "" {
			return "", false, true // nothing worth keeping
		}
		return "  [command output: " + util.TruncateMiddle(out, 700) + "]", false, true
	}
	if claudeInterruptRe.MatchString(strings.TrimSpace(text)) {
		return "  [assistant turn aborted by the user]", false, true
	}
	return "", false, false
}

// claudeTag returns the text inside the first <tag>…</tag> pair of s. Nested tags are not
// expected in these wrappers.
func claudeTag(s, tag string) (inside, rest string, ok bool) {
	open, close := "<"+tag+">", "</"+tag+">"
	if !strings.HasPrefix(s, open) {
		return "", "", false
	}
	end := strings.Index(s, close)
	if end < 0 {
		return "", "", false
	}
	return s[len(open):end], s[end+len(close):], true
}

func (c Claude) Open(f ir.SourceFile) (*ir.Session, error) {
	buf, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	return c.OpenBytes(f, buf, ScanOptions{})
}

// OpenBytes parses a transcript already read. ScanOptions are not used yet.
func (Claude) OpenBytes(f ir.SourceFile, buf []byte, _ ScanOptions) (*ir.Session, error) {
	read := parseJSONL(buf)
	s := &ir.Session{Kind: ir.Claude, File: f, CompleteBytes: read.completeBytes, BadLines: read.badLines}
	for _, r := range read.records {
		o := r.obj
		if s.SessionID == "" {
			s.SessionID = str(o, "sessionId")
		}
		if s.Cwd == "" {
			s.Cwd = str(o, "cwd")
		}
		if s.StartedAt == "" {
			s.StartedAt = str(o, "timestamp")
		}
	}
	if s.SessionID == "" {
		return nil, nil
	}
	// TODO(title): unverified whether Claude Code stores a session title in the transcript.

	s.Render = func(from, to int64) []ir.Line {
		var lines []ir.Line
		toolNames := map[string]string{}
		// TODO(branches): records are rendered in file order, as if the conversation were linear.
		// Rewinds / edit-and-resend most likely show up as a parentUuid that is not the previous
		// record; detect them like RenderOmp's cursor logic and emit the same "[branch: rewound …]"
		// note, once verified on a real session.
		for _, r := range read.records {
			o := r.obj
			emit := r.start >= from && (to < 0 || r.start < to)
			id := str(o, "uuid")
			if id == "" {
				id = fmt.Sprintf("L%d", r.line)
			}
			ts := str(o, "timestamp")
			push := func(role ir.Role, text string, turnStart bool, success *bool) {
				if emit && !util.IsSpaceOnly(text) {
					lines = append(lines, ir.Line{EntryID: id, TS: ts, Role: role, Text: util.Redact(text), TurnStart: turnStart, Start: r.start, End: r.end, Success: success})
				}
			}
			msg := obj(o, "message")
			switch str(o, "type") {
			case "user":
				if truthy(o["isMeta"]) {
					continue // injected by Claude Code, not typed by the user
				}
				said := strings.TrimSpace(contentText(msg["content"]))
				if note, turnStart, ok := claudeUserNote(said); ok {
					push(ir.RoleNote, note, turnStart, nil)
					continue
				}
				if said != "" {
					push(ir.RoleUser, "USER: "+util.TruncateMiddle(said, 4000), true, nil)
				}
				for _, b := range arr(msg["content"]) {
					bm, _ := b.(map[string]any)
					if str(bm, "type") != "tool_result" {
						continue
					}
					name := toolNames[str(bm, "tool_use_id")]
					if name == "" {
						name = "tool"
					}
					t := strings.TrimSpace(contentText(bm["content"]))
					switch {
					case truthy(bm["is_error"]):
						push(ir.RoleResult, fmt.Sprintf("  ✗ %s failed: %s", name, util.TruncateMiddle(t, 700)), false, ir.Bool(false))
					case bashLikeRe.MatchString(name):
						push(ir.RoleResult, fmt.Sprintf("  ✓ %s: %s", name, util.Tail(t, 300)), false, ir.Bool(true))
					default:
						suffix := ""
						if t != "" {
							suffix = ": " + util.Truncate(spaceRe.ReplaceAllString(t, " "), 120)
						}
						push(ir.RoleResult, fmt.Sprintf("  ✓ %s%s", name, suffix), false, ir.Bool(true))
					}
				}
			case "assistant":
				// One record per content block, so rendering block by block needs no merging by message.id.
				// TODO(stop): check how errors and user interrupts mark an assistant record (stop_reason?).
				for _, b := range arr(msg["content"]) {
					bm, _ := b.(map[string]any)
					switch str(bm, "type") {
					case "text":
						if t := strings.TrimSpace(str(bm, "text")); t != "" {
							push(ir.RoleAssistant, "ASSISTANT: "+util.TruncateMiddle(t, 2500), false, nil)
						}
					case "tool_use":
						if bid := str(bm, "id"); bid != "" {
							toolNames[bid] = str(bm, "name")
						}
						push(ir.RoleTool, fmt.Sprintf("  → %s(%s)", str(bm, "name"), SummarizeArgs(bm["input"])), false, nil)
					}
					// thinking: dropped, as for omp
				}
			}
			// TODO(compaction): find how a /compact summary is written and drop it like omp's
			// `compaction` (it repeats earlier content). Everything else is bookkeeping.
		}
		return lines
	}
	return s, nil
}
