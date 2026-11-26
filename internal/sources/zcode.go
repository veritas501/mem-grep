package sources

// ZCode adapter — read from the source of ZCode v4.0.0 (zcode-cli 0.18.0, 2026-11-23):
// apps/zcode-cli/packages/adapters/src/storage/session-store (schema.baseline.sql, rows.ts,
// repositories/messages.ts), packages/contracts/src/interfaces/session-store.port.ts,
// packages/shared/src/conversation-message-projection-policy.ts, packages/bootstrap/src/
// session-transcript.ts and active-transcript/index.ts, packages/core/src/memory; checked
// against a database and a memory file zcode 0.18.0 wrote itself (test/fixtures/zcode, made by
// gen.sh against a scripted mock model) and against the structure of a database zcode-cli
// 0.16.5 – 0.18.0 wrote in real use.
//
// Storage root:  $ZCODE_DATA_BASE_DIR/.zcode/cli (ZCODE_DATA_BASE_DIR defaults to the home
//                directory). Read-only here.
//
// Sessions:      db/db.sqlite, WAL mode. The schema descends from opencode's:
//                session  (id, parent_id, task_type, directory, workspace_id, title, revert JSON,
//                          time_created, time_updated, time_deleted, …)
//                message  (id, session_id, sequence, time_created, data JSON)
//                part     (id, message_id, session_id, sequence, time_created, data JSON)
//                Messages are read in ZCode's own order (sequence, then time_created); the parts
//                of an assistant message are reordered by blockIndex when every block has one.
//                task_type subagent_child / workflow_child / nested_workflow_child with a
//                parent_id is a subagent session; fork and selection_side_chat sessions also
//                have a parent_id but are the user's own conversations.
// Messages:      user {role, time, agent, synthetic?, source?, visibility?, semantics, summary?};
//                assistant {role, time, agent, parentID, error?, summary?, semantics, finish}.
//                semantics {origin, kind, source?, uiVisibility, providerVisibility,
//                transcriptVisibility} says who wrote a message and who may see it.
// Parts:         text {text, synthetic?, ignored?, blockIndex?} · reasoning · tool {callID, tool,
//                blockIndex?, state: {status: pending|running|completed|error, input,
//                output|error, metadata}} · file {mime, filename, url} · agent · subtask ·
//                compaction · timeline · step-start/finish · snapshot · patch · retry.
//
// What counts as the conversation is ZCode's own projection policy (zcodePolicy below is a port
// of getConversationMessageProjectionPolicy): a user message is the user only when its semantics
// say origin real_user and it is neither synthetic nor model-only; reminders, environment and
// date notes, skill and agent listings, todo reminders, background notifications, compaction
// summaries, fork notices and timeline events are runtime context and are dropped.
//
// A rewind does not delete anything: session.revert keeps keptMessageIDs (the prefix that
// survived) and branchCutAfterMessageID (the last message stored before the rewind). The active
// branch is the kept messages plus everything after the cut; the rest is the abandoned path,
// rendered in place and followed by a branch note.
//
// A failing Bash command is a "completed" tool part: its output starts with "Exit code N" and
// newer versions also set state.metadata.providerIsError.
//
// Memories:      memories/projects/<slug>-<hash16>/memory/**/*.md — MEMORY.md (the index) and
//                one Markdown file per memory with YAML front matter (name, description,
//                metadata: {type: user|feedback|project|reference, originSessionId}). <hash16> is
//                the first 16 hex digits of sha256(workspace identity, else workspace path), so
//                the project directory is recovered by hashing the sessions' workspace_id and
//                directory, or through originSessionId; the directory name is never reversed.
//
// Neither a session nor a memory is searched as a plain file. ReadRaw builds a stream of JSON
// lines — for a session a `session` line and one `message` line per message with its parts, for
// a memory a `memory` line and a `memory_body` line — and OpenBytes parses that; offsets are
// offsets in the stream. Only what can be rendered goes into the stream (no reasoning, step or
// snapshot parts, no file snapshots kept in tool metadata), and an interrupted message is marked
// `aborted` there, so the stream holds every word the transcript shows.
//
// zcode_test.go covers what a scripted run does not produce (rewinds, interrupts, errors,
// forks, deleted sessions, records without semantics, a memory index) on a database and a memory
// directory built from the source's shapes.
// TODO: a forked session repeats the turns copied from its parent; they are rendered again.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// ZCode is the ZCode adapter.
type ZCode struct{}

func (ZCode) Kind() ir.SourceKind { return ir.ZCode }

// zcodeMemoryPrefix starts the id of a memory file (the session ids are "sess_…").
const zcodeMemoryPrefix = "mem-"

// Sniff recognises a memory file by where it lives; sessions are never files of their own.
func (ZCode) Sniff(path string, _ []byte) bool {
	_, _, _, ok := zcodeMemoryPath(path)
	return ok
}

func (z ZCode) Discover(cfg *config.Config) []ir.SourceFile {
	var out []ir.SourceFile
	for _, root := range cfg.Dirs(ir.ZCode) {
		db, memories := zcodeLayout(root)
		files, _ := z.SessionsIn(db)
		for _, f := range files {
			if f.ParentKey != "" && !cfg.Subagents() {
				continue
			}
			out = append(out, f)
		}
		for _, f := range walkFiles(memories, isMarkdown, 8) {
			if mf, ok := zcodeMemoryFile(f.path, f.mtimeMs); ok {
				out = append(out, mf)
			}
		}
	}
	return out
}

func isMarkdown(name string) bool { return strings.HasSuffix(name, ".md") }

// zcodeLayout returns the session database and the memory directory of a storage root
// (…/.zcode/cli). The root may also be the database itself.
func zcodeLayout(root string) (db, memories string) {
	root = filepath.Clean(root)
	if st, err := os.Stat(root); err == nil && !st.IsDir() {
		return root, filepath.Join(filepath.Dir(filepath.Dir(root)), "memories", "projects")
	}
	return filepath.Join(root, "db", "db.sqlite"), filepath.Join(root, "memories", "projects")
}

// zcodeMemoryPath splits the path of a memory file: <cli root>/memories/projects/<project>/memory/<rel>.
func zcodeMemoryPath(path string) (cliRoot, project, rel string, ok bool) {
	if !isMarkdown(path) {
		return "", "", "", false
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i := len(parts) - 5; i >= 0; i-- {
		if parts[i] == "memories" && parts[i+1] == "projects" && parts[i+3] == "memory" {
			return filepath.FromSlash(strings.Join(parts[:i], "/")), parts[i+2], strings.Join(parts[i+4:], "/"), true
		}
	}
	return "", "", "", false
}

// zcodeMemoryFile describes a memory file. Its id is derived from the project directory and the
// file's path below it, so it is stable and distinct in the 13 characters a reference prints.
func zcodeMemoryFile(path string, mtimeMs float64) (ir.SourceFile, bool) {
	_, project, rel, ok := zcodeMemoryPath(path)
	if !ok {
		return ir.SourceFile{}, false
	}
	sum := sha256.Sum256([]byte(project + "/" + rel))
	return ir.SourceFile{Kind: ir.ZCode, Path: path, Key: path, Inner: zcodeMemoryPrefix + hex.EncodeToString(sum[:])[:12], MtimeMs: mtimeMs}, true
}

func zcodeIsMemory(f ir.SourceFile) bool { return strings.HasPrefix(f.Inner, zcodeMemoryPrefix) }

// zcodeSubagentTask reports the session kinds an agent starts for itself.
func zcodeSubagentTask(taskType string) bool {
	return taskType == "subagent_child" || taskType == "workflow_child" || taskType == "nested_workflow_child"
}

// SessionsIn lists the sessions of a ZCode database: one SourceFile each, Inner = session id.
// ok is false when the file is not a ZCode database (opencode's has no task_type column).
func (ZCode) SessionsIn(path string) ([]ir.SourceFile, bool) {
	if !isSQLite(path) {
		return nil, false
	}
	db, err := openDB(path)
	if err != nil {
		return nil, false
	}
	defer db.Close()
	// The first message's `agent` names a subagent ("zcode-Explore"); its title is only the
	// beginning of the task.
	rows, err := db.Query(`SELECT s.id, s.parent_id, s.task_type, s.time_updated,
		(SELECT CASE WHEN json_valid(m.data) THEN json_extract(m.data, '$.agent') END FROM message m
		 WHERE m.session_id = s.id ORDER BY m.sequence IS NULL, m.sequence, m.time_created, m.rowid LIMIT 1)
		FROM session s WHERE s.time_deleted IS NULL ORDER BY s.time_updated`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var out []ir.SourceFile
	for rows.Next() {
		var id, taskType string
		var parent, agent sql.NullString
		var updated int64
		if rows.Scan(&id, &parent, &taskType, &updated, &agent) != nil {
			continue
		}
		f := ir.SourceFile{Kind: ir.ZCode, Path: path, Key: path + "#" + id, Inner: id, MtimeMs: float64(updated)}
		if parent.String != "" && zcodeSubagentTask(taskType) {
			f.ParentKey = path + "#" + parent.String
			f.AgentName = agent.String
			if f.AgentName == "" {
				f.AgentName = "subagent"
			}
		}
		out = append(out, f)
	}
	return out, true
}

// ReadRaw builds the JSON-lines stream of one session or one memory (see the file comment).
func (ZCode) ReadRaw(f ir.SourceFile) ([]byte, error) {
	if zcodeIsMemory(f) {
		return zcodeMemoryStream(f)
	}
	return zcodeSessionStream(f)
}

// zcodeAborted reports an interrupted message: an assistant message whose error is a cancelled
// model request (core/src/runtime/helpers/turn-errors.ts), or the runtime's interrupt marker.
func zcodeAborted(data []byte) bool {
	var m struct {
		Source string `json:"source"`
		Error  *struct {
			Name string `json:"name"`
			Data struct {
				Code        string `json:"code"`
				Attribution struct {
					Reason string `json:"reason"`
				} `json:"attribution"`
			} `json:"data"`
		} `json:"error"`
		Semantics struct {
			Source string `json:"source"`
		} `json:"semantics"`
	}
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	if m.Source == "interrupt_marker" || m.Semantics.Source == "interrupt_marker" {
		return true
	}
	if m.Error == nil {
		return false
	}
	switch strings.ToLower(m.Error.Data.Code) {
	case "model_request_cancelled", "turn_cancelled", "abort_err":
		return true
	}
	return m.Error.Name == "AbortError" || m.Error.Data.Attribution.Reason == "cancelled"
}

func zcodeSessionStream(f ir.SourceFile) ([]byte, error) {
	db, err := openDB(f.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	var id, dir, title, taskType string
	var parent, revert sql.NullString
	var created, updated int64
	err = db.QueryRow(`SELECT id, parent_id, task_type, directory, title, revert, time_created, time_updated FROM session WHERE id = ?`, f.Inner).
		Scan(&id, &parent, &taskType, &dir, &title, &revert, &created, &updated)
	if err != nil {
		return nil, fmt.Errorf("zcode session %s: %w", f.Inner, err)
	}
	head := map[string]any{"type": "session", "id": id, "parent_id": parent.String, "task_type": taskType, "directory": dir, "title": title, "time_created": created, "time_updated": updated}
	if json.Valid([]byte(revert.String)) {
		head["revert"] = json.RawMessage(revert.String)
	}
	_ = enc.Encode(head)

	type part struct {
		ID   string          `json:"id"`
		Data json.RawMessage `json:"data"`
	}
	// Parts that are never rendered stay out; so do the file snapshots and display copies a tool
	// part keeps in its metadata (as large as the files the agent read).
	parts := map[string][]part{}
	prows, err := db.Query(`SELECT message_id, id, json_remove(data, '$.state.metadata.readFileState', '$.state.metadata.display', '$.state.attachments') FROM
		(SELECT * FROM part WHERE session_id = ? AND json_valid(data) ORDER BY message_id, sequence IS NULL, sequence, time_created, id)
		WHERE coalesce(json_extract(data, '$.type'), '') NOT IN ('reasoning', 'step-start', 'step-finish', 'snapshot', 'patch', 'retry')`, f.Inner)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var mid, pid, data string
		if prows.Scan(&mid, &pid, &data) == nil && json.Valid([]byte(data)) {
			parts[mid] = append(parts[mid], part{pid, json.RawMessage(data)})
		}
	}
	prows.Close()

	mrows, err := db.Query(`SELECT id, time_created, data FROM message WHERE session_id = ? ORDER BY sequence IS NULL, sequence, time_created, rowid`, f.Inner)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var mid, data string
		var t int64
		if mrows.Scan(&mid, &t, &data) != nil || !json.Valid([]byte(data)) {
			continue
		}
		ps := parts[mid]
		if ps == nil {
			ps = []part{}
		}
		_ = enc.Encode(struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Created int64           `json:"time_created"`
			Aborted bool            `json:"aborted,omitempty"`
			Data    json.RawMessage `json:"data"`
			Parts   []part          `json:"parts"`
		}{"message", mid, t, zcodeAborted([]byte(data)), json.RawMessage(data), ps})
	}
	return buf.Bytes(), mrows.Err()
}

// Open reads one session of the database, or one memory file.
func (z ZCode) Open(f ir.SourceFile) (*ir.Session, error) {
	if f.Inner == "" {
		// a memory file given by path without going through FileFor
		mf, ok := zcodeMemoryFile(f.Path, f.MtimeMs)
		if !ok {
			return nil, nil
		}
		f = mf
	}
	raw, err := z.ReadRaw(f)
	if err != nil {
		return nil, err
	}
	return z.OpenBytes(f, raw, ScanOptions{})
}

// OpenBytes parses a stream built by ReadRaw. ScanOptions are not used.
func (ZCode) OpenBytes(f ir.SourceFile, raw []byte, _ ScanOptions) (*ir.Session, error) {
	read := parseJSONL(raw)
	s := &ir.Session{Kind: ir.ZCode, File: f, CompleteBytes: read.completeBytes, BadLines: read.badLines}
	if zcodeIsMemory(f) {
		return openMemory(s, read), nil
	}
	var head map[string]any
	for _, r := range read.records {
		if str(r.obj, "type") == "session" {
			head = r.obj
			break
		}
	}
	if head == nil {
		return nil, nil
	}
	s.SessionID = zcodeSessionID(str(head, "id"))
	s.Cwd = str(head, "directory")
	s.Title = strings.TrimSpace(str(head, "title"))
	if ms, ok := head["time_created"].(float64); ok {
		s.StartedAt = millisISO(ms)
	}
	subagent := str(head, "parent_id") != "" && zcodeSubagentTask(str(head, "task_type"))
	lines := zcodeRender(read.records, obj(head, "revert"), subagent)
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

// zcodeSessionID is the id mg shows: the uuid without ZCode's "sess_" and "subagent_agent_"
// prefixes, which every subagent session shares for longer than a reference is.
func zcodeSessionID(id string) string {
	return strings.TrimPrefix(strings.TrimPrefix(id, "sess_"), "subagent_agent_")
}

// zcodeAbandoned returns the ids of the messages a rewind left behind: stored at or before the
// cut and not among the kept ones.
func zcodeAbandoned(records []record, revert map[string]any) map[string]bool {
	keptIDs, ok := revert["keptMessageIDs"].([]any)
	if !ok {
		return nil
	}
	kept := map[string]bool{}
	for _, k := range keptIDs {
		if id, ok := k.(string); ok {
			kept[id] = true
		}
	}
	var ids []string
	for _, r := range records {
		if str(r.obj, "type") == "message" {
			ids = append(ids, str(r.obj, "id"))
		}
	}
	// The cut is the last message of the old branch; older records name the first message of
	// the new one instead (createdMessageID). Without either, only the kept messages are active.
	cut := len(ids) - 1
	for i, id := range ids {
		if id == str(revert, "branchCutAfterMessageID") {
			cut = i
			break
		}
		if id == str(revert, "createdMessageID") {
			cut = i - 1
			break
		}
	}
	abandoned := map[string]bool{}
	for i := 0; i <= cut && i < len(ids); i++ {
		if !kept[ids[i]] {
			abandoned[ids[i]] = true
		}
	}
	return abandoned
}

func zcodeRender(records []record, revert map[string]any, subagent bool) []ir.Line {
	abandoned := zcodeAbandoned(records, revert)
	var lines []ir.Line
	leftBranch := false // an abandoned message was rendered since the last active one
	for _, r := range records {
		if str(r.obj, "type") != "message" {
			continue
		}
		msg := obj(r.obj, "data")
		id := str(r.obj, "id")
		ts := ""
		if ms, ok := obj(msg, "time")["created"].(float64); ok {
			ts = millisISO(ms)
		} else if ms, ok := r.obj["time_created"].(float64); ok {
			ts = millisISO(ms)
		}
		before := len(lines)
		push := func(role ir.Role, text string, turnStart bool, success *bool) {
			if !util.IsSpaceOnly(text) {
				lines = append(lines, ir.Line{EntryID: id, TS: ts, Role: role, Text: util.Redact(text), TurnStart: turnStart, Start: r.start, End: r.end, Success: success})
			}
		}
		if leftBranch && !abandoned[id] {
			push(ir.RoleNote, "[branch: the user rolled back earlier turns; what came after them was abandoned]", true, nil)
			leftBranch, before = false, len(lines)
		}
		var parts []map[string]any
		for _, p := range arr(r.obj["parts"]) {
			if pm := obj(mapOf(p), "data"); pm != nil {
				parts = append(parts, pm)
			}
		}
		aborted := truthy(r.obj["aborted"])
		switch zcodePolicy(msg, parts) {
		case zcodeUser:
			text := zcodeUserText(parts)
			if text == "" {
				break
			}
			if !subagent {
				pushUser(push, text)
			} else if name := str(msg, "agent"); name != "" {
				// in a subagent session the "user" is the agent that delegated the task
				push(ir.RoleTask, fmt.Sprintf("TASK for subagent %q: %s", name, util.TruncateMiddle(text, 3000)), true, nil)
			} else {
				push(ir.RoleTask, "TASK for subagent: "+util.TruncateMiddle(text, 3000), true, nil)
			}
		case zcodeAssistant:
			zcodeRenderAssistant(msg, parts, aborted, push)
		default:
			// The runtime's interrupt marker is a hidden user message; the cancelled assistant
			// message before it usually carried the note already.
			if aborted && (len(lines) == 0 || lines[len(lines)-1].Text != zcodeAbortNote) {
				push(ir.RoleNote, zcodeAbortNote, false, nil)
			}
		}
		if abandoned[id] && len(lines) > before {
			leftBranch = true
		}
	}
	return lines
}

const zcodeAbortNote = "  [assistant turn aborted by the user]"

// zcodeUserText joins what the user typed: text parts that are neither ignored nor synthetic,
// and the names of attached files.
func zcodeUserText(parts []map[string]any) string {
	var texts []string
	for _, pm := range parts {
		switch str(pm, "type") {
		case "text":
			if !truthy(pm["synthetic"]) && !truthy(pm["ignored"]) {
				if t := strings.TrimSpace(str(pm, "text")); t != "" {
					texts = append(texts, t)
				}
			}
		case "file":
			if name := str(pm, "filename"); name != "" {
				texts = append(texts, "[file: "+name+"]")
			}
		}
	}
	return strings.Join(texts, "\n")
}

var zcodeExitCodeRe = regexp.MustCompile(`^Exit code (\d+)`)

// zcodeBlockOrder reorders an assistant message's blocks (text and tool parts here; reasoning is
// not in the stream) by blockIndex, the order the model produced them, when every block has one;
// older messages keep their stored order (messages.ts orderAssistantBlockParts).
func zcodeBlockOrder(parts []map[string]any) []map[string]any {
	var blocks []map[string]any
	for _, pm := range parts {
		if t := str(pm, "type"); t == "text" || t == "tool" {
			if i, ok := pm["blockIndex"].(float64); !ok || i < 0 || i != float64(int64(i)) {
				return parts
			}
			blocks = append(blocks, pm)
		}
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i]["blockIndex"].(float64) < blocks[j]["blockIndex"].(float64) })
	out := make([]map[string]any, 0, len(parts))
	next := 0
	for _, pm := range parts {
		if t := str(pm, "type"); t == "text" || t == "tool" {
			pm = blocks[next]
			next++
		}
		out = append(out, pm)
	}
	return out
}

func zcodeRenderAssistant(msg map[string]any, parts []map[string]any, aborted bool, push pushFunc) {
	toolNames := map[string]string{}
	for _, pm := range zcodeBlockOrder(parts) {
		switch str(pm, "type") {
		case "text":
			if !truthy(pm["ignored"]) {
				pushAssistantText(push, str(pm, "text"))
			}
		case "tool":
			name := str(pm, "tool")
			state := obj(pm, "state")
			input := state["input"]
			if str(mapOf(input), "source") == "browser_turn_end" {
				continue // the screenshot ZCode takes at the end of a browser turn, not a call
			}
			pushToolCall(push, toolNames, str(pm, "callID"), name, input)
			switch str(state, "status") {
			case "completed":
				out := str(state, "output")
				failed := truthy(obj(state, "metadata")["providerIsError"])
				if m := zcodeExitCodeRe.FindStringSubmatch(out); m != nil && shellToolRe.MatchString(name) && strings.Trim(m[1], "0") != "" {
					failed = true // older versions did not set the flag
				}
				pushToolResult(push, name, out, failed)
			case "error":
				pushToolResult(push, name, str(state, "error"), true)
			}
			// pending / running: no result (yet)
		}
		// file, agent, subtask, compaction, timeline: not part of the story
	}
	switch e := obj(msg, "error"); {
	case aborted:
		push(ir.RoleNote, zcodeAbortNote, false, nil)
	case e != nil:
		m := str(obj(e, "data"), "message")
		if m == "" {
			m = str(e, "name")
		}
		if m != "" {
			push(ir.RoleNote, "  [assistant error: "+util.Truncate(m, 300)+"]", false, nil)
		}
	}
}

// The outcome of ZCode's projection policy, reduced to what mg renders.
const (
	zcodeHidden = iota
	zcodeUser
	zcodeAssistant
)

// Sources of synthetic user messages that only feed the model (PROVIDER_CONTEXT_SYNTHETIC_SOURCES).
var zcodeContextSources = map[string]bool{
	"agent_listing_delta": true, "agent_control_message": true, "background_task": true, "goal-continuation": true,
	"goal_completion_verification": true, "goal_state_change": true, "plugin_reference": true, "bot_topic_context": true,
	"queued_system_notification": true, "resume_goal_state": true, "resume_referenced_session_context": true,
	"rewind": true, "selection_side_chat": true, "subagent": true, "subagent_message": true, "target_continuation": true,
	"task_notification": true, "task_status": true, "todo_reminder": true, "fork": true,
}

// zcodePolicy ports getConversationMessageProjectionPolicy (packages/shared/src/
// conversation-message-projection-policy.ts): realUserInput is the user, visibleAssistant the
// agent; providerContextOnly, timelineOnly and hiddenSynthetic are all hidden here.
func zcodePolicy(info map[string]any, parts []map[string]any) int {
	sem := obj(info, "semantics")
	if _, summary := info["summary"]; summary || str(sem, "kind") == "compact_summary" {
		return zcodeHidden // a compaction summary repeats earlier turns
	}
	role := str(info, "role")
	synthetic := info["synthetic"] == true
	modelOnly := str(info, "visibility") == "model-only"
	if sem != nil {
		kind, ui, transcript := str(sem, "kind"), str(sem, "uiVisibility"), str(sem, "transcriptVisibility")
		switch {
		case kind == "timeline_event" && ui != "hidden":
			return zcodeHidden
		case str(sem, "origin") == "real_user" && !synthetic && !modelOnly:
			return zcodeUser
		case role == "assistant" && kind == "assistant_response" && ui == "visible" && transcript == "visible":
			return zcodeAssistant
		case str(sem, "providerVisibility") == "visible", kind == "fork_notice",
			str(sem, "origin") == "agent_runtime", ui == "hidden", transcript == "hidden":
			return zcodeHidden
		}
	}
	// Records without usable semantics (older versions): decide from the legacy markers.
	if modelOnly || str(info, "source") == "fork" {
		return zcodeHidden
	}
	source := str(info, "source")
	if source == "" {
		source = str(obj(info, "metadata"), "source")
	}
	if source == "" {
		source = str(sem, "source")
	}
	var text strings.Builder
	for _, pm := range parts {
		md := obj(pm, "metadata")
		if str(md, "visibility") == "model-only" || str(md, "source") == "goal-continuation" {
			return zcodeHidden
		}
		if t := str(pm, "type"); t == "timeline" || str(obj(md, "forkContext"), "kind") == "session_fork" ||
			(t == "compaction" && (str(md, "timelineStatus") != "" || str(pm, "summaryMessageId") != "")) {
			return zcodeHidden
		}
		if source == "" {
			source = str(md, "source")
		}
		synthetic = synthetic || pm["synthetic"] == true
		if str(pm, "type") == "text" && pm["ignored"] != true {
			text.WriteString(str(pm, "text"))
		}
	}
	t := strings.TrimLeft(text.String(), " \t\r\n")
	switch {
	case zcodeContextSources[source], synthetic:
		return zcodeHidden
	case strings.HasPrefix(t, `<system-reminder source="goal-continuation">`),
		strings.HasPrefix(t, "<system-reminder>") && (strings.Contains(t, "Continue working toward the active session goal.") || strings.Contains(t, "Current session goal state")),
		strings.Contains(t, "Conversation rewind applied."), strings.Contains(t, "Workspace rewind applied."):
		return zcodeHidden
	}
	if role == "assistant" {
		return zcodeAssistant
	}
	return zcodeUser
}

// ---------------------------------------------------------------------------
// Memories
// ---------------------------------------------------------------------------

// zcodeProjects maps a memory directory's hash to the working directory it belongs to, for the
// sessions of one database. It lives for the process only: mg keeps nothing between calls.
var zcodeProjects sync.Map // database path -> map[string]string

func zcodeProjectDirs(dbPath string) map[string]string {
	if m, ok := zcodeProjects.Load(dbPath); ok {
		return m.(map[string]string)
	}
	dirs := map[string]string{}
	if isSQLite(dbPath) {
		if db, err := openDB(dbPath); err == nil {
			if rows, err := db.Query(`SELECT DISTINCT coalesce(workspace_id, ''), directory FROM session`); err == nil {
				for rows.Next() {
					var workspace, dir string
					if rows.Scan(&workspace, &dir) != nil || dir == "" {
						continue
					}
					// resolveProjectMemoryRoot hashes the workspace identity when there is one,
					// else the workspace path (lower-cased on Windows).
					for _, key := range []string{workspace, dir, strings.ToLower(dir)} {
						if key != "" {
							sum := sha256.Sum256([]byte(key))
							if h := hex.EncodeToString(sum[:])[:16]; dirs[h] == "" {
								dirs[h] = dir
							}
						}
					}
				}
				rows.Close()
			}
			db.Close()
		}
	}
	zcodeProjects.Store(dbPath, dirs)
	return dirs
}

// zcodeSessionDir is the working directory of a session, "" when unknown.
func zcodeSessionDir(dbPath, sessionID string) string {
	if sessionID == "" || !isSQLite(dbPath) {
		return ""
	}
	db, err := openDB(dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	var dir string
	_ = db.QueryRow(`SELECT directory FROM session WHERE id = ?`, sessionID).Scan(&dir)
	return dir
}

// zcodeMemoryStream builds the two lines of a memory: what the file says about itself and where
// it belongs, then its body.
func zcodeMemoryStream(f ir.SourceFile) ([]byte, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	cliRoot, project, rel, ok := zcodeMemoryPath(f.Path)
	if !ok {
		return nil, ErrUnknownFormat
	}
	meta, body := memoryFrontMatter(string(data))
	dbPath := filepath.Join(cliRoot, "db", "db.sqlite")
	cwd := ""
	if i := strings.LastIndexByte(project, '-'); i >= 0 {
		cwd = zcodeProjectDirs(dbPath)[project[i+1:]]
	}
	if cwd == "" {
		cwd = zcodeSessionDir(dbPath, meta["originSessionId"])
	}
	mtime := f.MtimeMs
	if st, err := os.Stat(f.Path); err == nil {
		mtime = float64(st.ModTime().UnixNano()) / 1e6
	}
	return memoryStream(memoryRecord{ID: f.Inner, File: rel, Project: project, Cwd: cwd, MtimeMs: mtime, Name: meta["name"],
		Description: meta["description"], Type: meta["type"], OriginSession: meta["originSessionId"], Body: body}), nil
}
