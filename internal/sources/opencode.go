package sources

// opencode adapter — read from the source of sst/opencode at release v1.19.5 (2026-10-28):
// packages/core/src/session/sql.ts, packages/schema/src/v1/session.ts, packages/core/src/database/
// database.ts, and checked against a database opencode-ai 1.19.5 wrote itself
// (test/fixtures/opencode/opencode.db, made by gen.sh against a scripted mock model).
//
// Storage:       one SQLite database, $XDG_DATA_HOME/opencode/opencode.db (~/.local/share/opencode),
//                WAL mode; dev channels use opencode-<channel>.db. Read-only here.
// Tables:        session  (id, project_id, parent_id, directory, title, time_created, time_updated, …)
//                message  (id, session_id, time_created, data JSON: {role, time, agent, model, …})
//                part     (id, message_id, session_id, time_created, data JSON: {type, …})
//                parent_id marks a subagent session (the task tool). Message and part ids ascend
//                with time ("msg_…", "prt_…").
// Parts:         text {text, synthetic?, ignored?} · reasoning · tool {callID, tool, state:
//                {status: pending|running|completed|error, input, output|error}} · file {mime,
//                filename, url} · subtask · agent · compaction · step-start/finish · snapshot ·
//                patch · retry. A user message holds text/file parts; an assistant message
//                {role: "assistant", error?, summary?, path: {cwd, root}} holds the rest.
//
// A database holds many sessions, so a session is not a file. ReadRaw builds a stream of JSON
// lines for one session — a `session` line, then one `message` line per message carrying its parts
// — and OpenBytes parses that. Offsets are offsets in the stream (sessions are append-only in
// practice; a tool part that completes after its message was read is picked up only when the
// session is read from scratch again). mg prefilters the same bytes.
//
// What the real database showed: the tables and columns are as described; a user message made by
// `opencode run` is wrapped in literal double quotes; the bash tool finishes "completed" whatever the
// command did, the exit code is in state.metadata.exit; a subagent session has parent_id and a title
// "<description> (@<agent> subagent)". opencode_test.go covers cases a scripted run does not produce
// (error states, aborts, synthetic text, attachments, compaction summaries) on a hand-built database.
// TODO(v2): the main branch has begun a session_v2 / session_message schema; not read.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// OpenCode is the opencode adapter.
type OpenCode struct{}

func (OpenCode) Kind() ir.SourceKind { return ir.OpenCode }

// Sniff is false: an opencode session is never a file of its own (see SessionsIn).
func (OpenCode) Sniff(string, []byte) bool { return false }

var sqliteMagic = []byte("SQLite format 3\x00")

func isSQLite(path string) bool {
	head, err := readHead(path, len(sqliteMagic))
	return err == nil && bytes.Equal(head, sqliteMagic)
}

func (o OpenCode) Discover(cfg *config.Config) []ir.SourceFile {
	var out []ir.SourceFile
	for _, root := range cfg.Dirs(ir.OpenCode) {
		for _, db := range openCodeDatabases(root) {
			files, _ := o.SessionsIn(db)
			for _, f := range files {
				if f.ParentKey != "" && !cfg.Subagents() {
					continue
				}
				out = append(out, f)
			}
		}
	}
	return out
}

// openCodeDatabases lists opencode*.db in a data directory (or a database given directly).
func openCodeDatabases(root string) []string {
	st, err := os.Stat(root)
	if err != nil {
		return nil
	}
	if !st.IsDir() {
		return []string{root}
	}
	matches, _ := filepath.Glob(filepath.Join(root, "opencode*.db"))
	return matches
}

var (
	dbMu    sync.Mutex
	dbCache = map[string]*sql.DB{}
)

// openDB opens a database read-only once per process; the handle is shared by all searches.
func openDB(path string) (*sql.DB, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if db, ok := dbCache[path]; ok {
		return db, nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	dbCache[path] = db
	return db, nil
}

// SessionsIn lists the sessions of an opencode database: one SourceFile each, Inner = session id.
// ok is false when the file is not an opencode database.
func (OpenCode) SessionsIn(path string) ([]ir.SourceFile, bool) {
	if !isSQLite(path) {
		return nil, false
	}
	db, err := openDB(path)
	if err != nil {
		return nil, false
	}
	rows, err := db.Query(`SELECT id, parent_id, title, time_updated FROM session ORDER BY time_updated`)
	if err != nil {
		return nil, false // no session table: some other SQLite file
	}
	defer rows.Close()
	var out []ir.SourceFile
	for rows.Next() {
		var id, title string
		var parent sql.NullString
		var updated int64
		if rows.Scan(&id, &parent, &title, &updated) != nil {
			continue
		}
		f := ir.SourceFile{Kind: ir.OpenCode, Path: path, Key: path + "#" + id, Inner: id, MtimeMs: float64(updated)}
		if parent.Valid && parent.String != "" {
			f.ParentKey = path + "#" + parent.String
			f.AgentName = title
		}
		out = append(out, f)
	}
	return out, true
}

// ReadRaw builds the JSON-lines stream of one session (see the file comment).
func (OpenCode) ReadRaw(f ir.SourceFile) ([]byte, error) {
	db, err := openDB(f.Path)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	var id, dir, title string
	var parent sql.NullString
	var created, updated int64
	err = db.QueryRow(`SELECT id, parent_id, directory, title, time_created, time_updated FROM session WHERE id = ?`, f.Inner).
		Scan(&id, &parent, &dir, &title, &created, &updated)
	if err != nil {
		return nil, fmt.Errorf("opencode session %s: %w", f.Inner, err)
	}
	_ = enc.Encode(map[string]any{"type": "session", "id": id, "parent_id": parent.String, "directory": dir, "title": title, "time_created": created, "time_updated": updated})

	type part struct {
		ID   string          `json:"id"`
		Data json.RawMessage `json:"data"`
	}
	parts := map[string][]part{}
	prows, err := db.Query(`SELECT message_id, id, data FROM part WHERE session_id = ? ORDER BY message_id, id`, f.Inner)
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

	mrows, err := db.Query(`SELECT id, time_created, data FROM message WHERE session_id = ? ORDER BY time_created, id`, f.Inner)
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
			Data    json.RawMessage `json:"data"`
			Parts   []part          `json:"parts"`
		}{"message", mid, t, json.RawMessage(data), ps})
	}
	return buf.Bytes(), mrows.Err()
}

// Open reads one session of the database.
func (o OpenCode) Open(f ir.SourceFile) (*ir.Session, error) {
	raw, err := o.ReadRaw(f)
	if err != nil {
		return nil, err
	}
	return o.OpenBytes(f, raw, ScanOptions{})
}

// OpenBytes parses a stream built by ReadRaw. ScanOptions are not used.
func (OpenCode) OpenBytes(f ir.SourceFile, raw []byte, _ ScanOptions) (*ir.Session, error) {
	read := parseJSONL(raw)
	s := &ir.Session{Kind: ir.OpenCode, File: f, CompleteBytes: read.completeBytes, BadLines: read.badLines}
	subagent := false
	for _, r := range read.records {
		switch str(r.obj, "type") {
		case "session":
			subagent = str(r.obj, "parent_id") != ""
			s.SessionID = str(r.obj, "id")
			s.Cwd = str(r.obj, "directory")
			s.Title = strings.TrimSpace(str(r.obj, "title"))
			if ms, ok := r.obj["time_created"].(float64); ok {
				s.StartedAt = millisISO(ms)
			}
		}
	}
	if s.SessionID == "" {
		return nil, nil
	}
	s.Render = func(from, to int64) []ir.Line {
		var lines []ir.Line
		tasked := false
		for _, r := range read.records {
			if str(r.obj, "type") != "message" || r.start < from || (to >= 0 && r.start >= to) {
				continue
			}
			msg := obj(r.obj, "data")
			ts := ""
			if ms, ok := obj(msg, "time")["created"].(float64); ok {
				ts = millisISO(ms)
			}
			id := str(r.obj, "id")
			push := func(role ir.Role, text string, turnStart bool, success *bool) {
				if !util.IsSpaceOnly(text) {
					lines = append(lines, ir.Line{EntryID: id, TS: ts, Role: role, Text: util.Redact(text), TurnStart: turnStart, Start: r.start, End: r.end, Success: success})
				}
			}
			if subagent && str(msg, "role") == "user" {
				// the user message of a subagent session is the task its parent delegated
				if text := openCodeUserText(arr(r.obj["parts"])); text != "" && !tasked {
					tasked = true
					push(ir.RoleTask, "TASK for subagent: "+util.TruncateMiddle(text, 3000), true, nil)
				}
				continue
			}
			renderOpenCodeMessage(msg, arr(r.obj["parts"]), push)
		}
		return lines
	}
	return s, nil
}

func millisISO(ms float64) string {
	return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05.000Z")
}

// openCodeUserText joins what the user typed: text parts that are neither synthetic (injected by
// opencode, e.g. the result of an @file mention) nor ignored, and the names of attached files.
func openCodeUserText(parts []any) string {
	var texts []string
	for _, p := range parts {
		pm := obj(mapOf(p), "data")
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

func mapOf(v any) map[string]any { m, _ := v.(map[string]any); return m }

func renderOpenCodeMessage(msg map[string]any, parts []any, push pushFunc) {
	switch str(msg, "role") {
	case "user":
		pushUser(push, openCodeUserText(parts))
	case "assistant":
		if truthy(msg["summary"]) {
			return // the compaction summary repeats earlier turns
		}
		toolNames := map[string]string{}
		for _, p := range parts {
			part := mapOf(p)
			pm := obj(part, "data")
			switch str(pm, "type") {
			case "text":
				if !truthy(pm["synthetic"]) && !truthy(pm["ignored"]) {
					pushAssistantText(push, str(pm, "text"))
				}
			case "tool":
				name := str(pm, "tool")
				state := obj(pm, "state")
				pushToolCall(push, toolNames, str(pm, "callID"), name, state["input"])
				switch str(state, "status") {
				case "completed":
					// The bash tool finishes "completed" whatever the command did; its exit code is in the
					// metadata (checked on 1.19.5).
					out, failed := str(state, "output"), false
					if code, ok := obj(state, "metadata")["exit"].(float64); ok && code != 0 {
						failed = true
						out = strings.TrimSpace(out) + fmt.Sprintf(" (exit %d)", int(code))
					}
					pushToolResult(push, name, out, failed)
				case "error":
					pushToolResult(push, name, str(state, "error"), true)
				}
				// pending / running: no result (yet)
			}
			// reasoning, step-start/finish, snapshot, patch, retry, agent, subtask, compaction: not part of the story
		}
		if e := obj(msg, "error"); e != nil {
			if str(e, "name") == "MessageAbortedError" {
				push(ir.RoleNote, "  [assistant turn aborted by the user]", false, nil)
			} else if m := str(obj(e, "data"), "message"); m != "" {
				push(ir.RoleNote, "  [assistant error: "+util.Truncate(m, 300)+"]", false, nil)
			}
		}
	}
}
