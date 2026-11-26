package sources

// Antigravity adapter — Google Antigravity's CLI (agy) and IDE. There is no source to read: the
// format was taken from what agy 1.5.x writes (checked against 31 real conversations) and from
// the protobuf descriptors embedded in the agy binary (exa.cortex_pb, gemini_coder.Step), which
// name the step types and statuses below.
//
// Storage root:  ~/.gemini/antigravity-cli (the CLI) and ~/.gemini/antigravity (the IDE); agy
//                moves the whole ~/.gemini with its --gemini_dir flag. Read-only here.
//
// Every conversation is stored three times:
//   conversations/<id>.db   SQLite, one row of protobuf per step. Not read: once a conversation
//                           has been compacted, agy clears the older steps (status CLEARED) and
//                           keeps only the user's input, the reply text and shell output; tool
//                           arguments, other tool results and thinking are gone.
//   brain/<id>/.system_generated/logs/transcript_full.jsonl
//                           the complete transcript, one JSON object per step, append-only. This
//                           is what mg reads. transcript.jsonl beside it is the same with long
//                           fields cut (truncated_fields) and arguments turned into strings;
//                           chunks/ splits transcript_full.jsonl into pieces. Neither is read.
//   conversation_summaries.db   one row per conversation: title, workspace_uris (the working
//                           directory), parent_conversation_id and agent_name for subagents.
// A conversation with no steps has no transcript.
//
// A step is {step_index, source, type, status, created_at, content?, thinking?, tool_calls?,
// error?, model?, token counts}:
//   USER_INPUT       the user's message in <USER_REQUEST>…</USER_REQUEST>, followed by what the
//                    CLI injects (<ADDITIONAL_METADATA>, <USER_SETTINGS_CHANGE>, <SKILL>)
//   PLANNER_RESPONSE the model's reply (content) and tool calls [{name, args}] with no call ids;
//                    status ERROR with `error` when the request failed (e.g. a safety filter)
//   GENERIC          one tool result, in the order of the calls before it; content starts with
//                    "Created At:" / "Completed At:" lines (twice). A shell command reports
//                    "Command exited with code N."; a tool that failed reports "Encountered
//                    error in step execution: …" and has status ERROR.
//   SYSTEM_MESSAGE   runtime reminders, and messages between agents:
//                    "[Message] timestamp=… sender=<conversation id>[/task-N] priority=… content=…".
//                    A subagent's task arrives this way from its parent; a background command's
//                    result arrives from "<own id>/task-N".
//   ERROR_MESSAGE    retry notices for the model (dropped; the failed step carries the error)
//   CHECKPOINT       a compaction summary (dropped)
// Rewinds: when the user goes back and sends again, agy appends the new path with step indexes
// that were already used. A step_index not above the previous one starts a new branch; what came
// before stays rendered and is followed by a branch note. A step that was cancelled is never
// written, so the indexes can skip.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// Antigravity is the Antigravity (agy) adapter.
type Antigravity struct{}

func (Antigravity) Kind() ir.SourceKind { return ir.Antigravity }

const antigravityTranscript = "transcript_full.jsonl"

// antigravityTranscriptPath splits <root>/brain/<id>/.system_generated/logs/transcript_full.jsonl.
func antigravityTranscriptPath(path string) (root, id string, ok bool) {
	logs := filepath.Dir(path)
	gen := filepath.Dir(logs)
	conv := filepath.Dir(gen)
	brain := filepath.Dir(conv)
	if filepath.Base(path) != antigravityTranscript || filepath.Base(logs) != "logs" ||
		filepath.Base(gen) != ".system_generated" || filepath.Base(brain) != "brain" {
		return "", "", false
	}
	return filepath.Dir(brain), filepath.Base(conv), true
}

// Sniff recognises a transcript by where it lives and by its first step.
func (Antigravity) Sniff(path string, head []byte) bool {
	if _, _, ok := antigravityTranscriptPath(path); !ok {
		return false
	}
	h := firstLine(bytes.TrimSpace(head))
	return len(h) == 0 || (bytes.Contains(h, []byte(`"step_index"`)) && bytes.Contains(h, []byte(`"type"`)))
}

func (a Antigravity) Discover(cfg *config.Config) []ir.SourceFile {
	var out []ir.SourceFile
	for _, root := range cfg.Dirs(ir.Antigravity) {
		root = filepath.Clean(root)
		brain := filepath.Join(root, "brain")
		entries, err := os.ReadDir(brain)
		if err != nil {
			continue
		}
		index := antigravityIndex(root)
		transcript := func(id string) string {
			return filepath.Join(brain, id, ".system_generated", "logs", antigravityTranscript)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := transcript(e.Name())
			st, err := os.Stat(path)
			if err != nil {
				continue // a conversation without steps
			}
			f := ir.SourceFile{Kind: ir.Antigravity, Path: path, Key: path, Size: st.Size(), MtimeMs: float64(st.ModTime().UnixNano()) / 1e6}
			if info := index[e.Name()]; info.parent != "" {
				parent := transcript(info.parent)
				if _, err := os.Stat(parent); err == nil {
					if !cfg.Subagents() {
						continue
					}
					f.ParentKey = parent
					f.AgentName = info.agent
					if f.AgentName == "" {
						f.AgentName = "subagent"
					}
				}
			}
			out = append(out, f)
		}
	}
	return out
}

// antigravityConversation is what conversation_summaries.db knows about a conversation.
type antigravityConversation struct {
	title, cwd, parent, agent string
}

var antigravityIndexes = struct {
	sync.Mutex
	byDB map[string]antigravityIndexEntry
}{byDB: map[string]antigravityIndexEntry{}}

type antigravityIndexEntry struct {
	mtime int64
	convs map[string]antigravityConversation
}

// antigravityIndex reads conversation_summaries.db of a storage root, once per change of the file.
// It is empty when the database is missing or unreadable: the transcripts are still searched,
// without titles, working directories or subagent links.
func antigravityIndex(root string) map[string]antigravityConversation {
	path := filepath.Join(root, "conversation_summaries.db")
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	antigravityIndexes.Lock()
	defer antigravityIndexes.Unlock()
	if e, ok := antigravityIndexes.byDB[path]; ok && e.mtime == st.ModTime().UnixNano() {
		return e.convs
	}
	convs := map[string]antigravityConversation{}
	if db, err := openDB(path); err == nil {
		rows, err := db.Query(`SELECT conversation_id, title, workspace_uris, parent_conversation_id, agent_name FROM conversation_summaries`)
		if err == nil {
			for rows.Next() {
				var id, title, uris, parent, agent sql.NullString
				if rows.Scan(&id, &title, &uris, &parent, &agent) == nil {
					convs[id.String] = antigravityConversation{title: title.String, cwd: antigravityWorkspace(uris.String), parent: parent.String, agent: agent.String}
				}
			}
			rows.Close()
		}
		db.Close()
	}
	antigravityIndexes.byDB[path] = antigravityIndexEntry{st.ModTime().UnixNano(), convs}
	return convs
}

// antigravityWorkspace turns workspace_uris (a JSON array of file:// URIs) into the first path.
func antigravityWorkspace(uris string) string {
	var list []string
	if json.Unmarshal([]byte(uris), &list) != nil {
		return ""
	}
	for _, u := range list {
		p, err := url.Parse(u)
		if err != nil || p.Scheme != "file" {
			continue
		}
		path := p.Path
		if len(path) > 2 && path[0] == '/' && path[2] == ':' {
			// file:///C:/… is a Windows path; anything else (a POSIX path, also from WSL or a
			// remote workspace) is kept as written, whatever system mg runs on
			return filepath.FromSlash(path[1:])
		}
		return path
	}
	return ""
}

func (a Antigravity) Open(f ir.SourceFile) (*ir.Session, error) {
	buf, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	return a.OpenBytes(f, buf, ScanOptions{})
}

// OpenBytes parses a transcript. ScanOptions are not used.
func (Antigravity) OpenBytes(f ir.SourceFile, raw []byte, _ ScanOptions) (*ir.Session, error) {
	root, id, ok := antigravityTranscriptPath(f.Path)
	if !ok {
		return nil, nil
	}
	read := parseJSONL(raw)
	s := &ir.Session{Kind: ir.Antigravity, File: f, SessionID: id, CompleteBytes: read.completeBytes, BadLines: read.badLines}
	info := antigravityIndex(root)[id]
	s.Title, s.Cwd = strings.TrimSpace(info.title), info.cwd
	for _, r := range read.records {
		if ts := str(r.obj, "created_at"); ts != "" {
			s.StartedAt = ts
			break
		}
	}
	lines := antigravityRender(read.records, id, info.parent)
	s.Render = func(from, to int64) []ir.Line {
		var out []ir.Line
		for _, l := range lines {
			if l.Start >= from && (to < 0 || l.Start < to) {
				out = append(out, l)
			}
		}
		return out
	}
	return s, nil
}

var (
	antigravityRequestRe = regexp.MustCompile(`(?s)<USER_REQUEST>\n?(.*?)\n?</USER_REQUEST>`)
	antigravityMessageRe = regexp.MustCompile(`(?s)\[Message\] timestamp=\S+ sender=(\S+) priority=\S+ content=(.*?)\s*(?:</SYSTEM_MESSAGE>|$)`)
	antigravityExitRe    = regexp.MustCompile(`^Command exited with code (-?\d+)\.`)
)

const antigravityStepError = "Encountered error in step execution: "

// antigravityRender renders the steps of one transcript. parent is the conversation that started
// this one when it is a subagent.
func antigravityRender(records []record, id, parent string) []ir.Line {
	var lines []ir.Line
	var pending []string // tool names of the last reply, waiting for their results in order
	last := -1           // step_index of the previous step on the current path
	taskSeen := false
	for _, r := range records {
		step, ok := r.obj["step_index"].(float64)
		if !ok {
			continue
		}
		ts := str(r.obj, "created_at")
		push := func(role ir.Role, text string, turnStart bool, success *bool) {
			if !util.IsSpaceOnly(text) {
				lines = append(lines, ir.Line{EntryID: strconv.Itoa(r.line), TS: ts, Role: role, Text: util.Redact(text), TurnStart: turnStart, Start: r.start, End: r.end, Success: success})
			}
		}
		if int(step) <= last && len(lines) > 0 {
			push(ir.RoleNote, "[branch: the user rolled back earlier turns; what came after them was abandoned]", true, nil)
			pending = nil
		}
		last = int(step)

		content := str(r.obj, "content")
		switch str(r.obj, "type") {
		case "USER_INPUT":
			text := content
			if m := antigravityRequestRe.FindStringSubmatch(content); m != nil {
				text = m[1]
			}
			pushUser(push, text)
			pending = nil
		case "PLANNER_RESPONSE":
			pushAssistantText(push, content)
			pending = pending[:0]
			for _, c := range arr(r.obj["tool_calls"]) {
				call := mapOf(c)
				name := str(call, "name")
				pushToolCall(push, map[string]string{}, "", name, call["args"])
				pending = append(pending, name)
			}
			if str(r.obj, "status") == "ERROR" {
				msg := str(r.obj, "error")
				if msg == "" {
					msg = "unknown"
				}
				push(ir.RoleNote, "  [assistant error: "+util.TruncateMiddle(msg, 500)+"]", false, nil)
			}
		case "GENERIC":
			name := "tool"
			if len(pending) > 0 {
				name, pending = pending[0], pending[1:]
			}
			text, failed := antigravityResult(content)
			if str(r.obj, "status") == "ERROR" {
				failed = true
				if text == "" {
					text = str(r.obj, "error")
				}
			}
			pushToolResult(push, name, text, failed)
		case "SYSTEM_MESSAGE":
			m := antigravityMessageRe.FindStringSubmatch(content)
			if m == nil {
				continue // a runtime reminder
			}
			sender, text := m[1], strings.TrimSpace(m[2])
			switch {
			case parent != "" && sender == parent && !taskSeen:
				taskSeen = true
				push(ir.RoleTask, "TASK for subagent: "+util.TruncateMiddle(text, 3000), true, nil)
			case strings.HasPrefix(sender, id+"/"):
				// the result of a command the agent ran in the background
				push(ir.RoleNote, "  [command output: "+util.TruncateMiddle(text, 700)+"]", false, nil)
			case sender != "system":
				push(ir.RoleNote, "[message from another agent: "+util.TruncateMiddle(text, 1200)+"]", false, nil)
			}
		}
	}
	return lines
}

// antigravityResult strips the timing lines of a tool result and tells whether it failed: a step
// error, or a shell command that exited non-zero. A shell command keeps only its output when it
// succeeded.
func antigravityResult(content string) (text string, failed bool) {
	rest := content
	for {
		line, after, found := strings.Cut(rest, "\n")
		if !strings.HasPrefix(line, "Created At: ") && !strings.HasPrefix(line, "Completed At: ") {
			break
		}
		rest = after
		if !found {
			break
		}
	}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, antigravityStepError) {
		return strings.TrimPrefix(rest, antigravityStepError), true
	}
	if m := antigravityExitRe.FindStringSubmatch(rest); m != nil {
		if m[1] != "0" {
			return rest, true
		}
		if _, out, ok := strings.Cut(rest, "Command output:\n"); ok {
			return out, false
		}
	}
	return rest, false
}
