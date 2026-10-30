package sources

// Gemini CLI adapter — read from the source of google-gemini/gemini-cli (packages/core/src/services/
// chatRecordingService.ts and chatRecordingTypes.ts, main as of 2026-10-28) and checked against
// sessions Gemini CLI 0.66.0 wrote itself (test/fixtures/gemini, made by gen.sh against a scripted
// mock model).
//
// Layout:        ~/.gemini/tmp/<project id>/chats/session-<YYYY-MM-DDTHH-MM>-<id8>.jsonl
//                <project id> is a short slug from ~/.gemini/projects.json (older versions: the
//                sha256 of the project root); the directory holds `.project_root` with the real path.
// Subagents:     <project id>/chats/<parent session id>/<subagent session id>.jsonl (kind "subagent")
// Legacy:        session-*.json holding one whole ConversationRecord {…, messages: […]}.
//
// File format (JSONL, append-only). A line is one of:
//   {sessionId, projectHash, startTime, lastUpdated, kind?, directories?, summary?}   metadata
//   {id, timestamp, type: user|gemini|info|error|warning, content, …}                  a message;
//       gemini messages also carry toolCalls[{id, name, args, result, status}], thoughts, tokens
//   {$set: {…}}                          metadata update (legacy: {messages: […]} replaces all)
//   {$patch: {id, content?, toolCalls?[{id, result}], updates?[…], removeIds?[…], orderIds?[…]}}
//   {$rewindTo: <message id>}            drops that message and everything after it
// A message line with a known id replaces the message in place. The final conversation is the
// replay of all lines; tool results arrive late through $patch.
//
// What the real files showed beyond the types: a `gemini` message is written again under the same id
// once its tool calls have results; a tool's reply is also stored as a separate `user` message of
// functionResponse parts (not rendered: the result is on the tool call); the first user message is
// the CLI's <session_context>; the shell tool wraps its report in <untrusted_context> and a failed
// command still has status "success" with an "Exit Code: N" line; a subagent file's first user
// message is its task and the later ones are the loop's nudges.
//
// Known gaps (gemini_test.go covers $patch / $rewindTo / legacy files on small records, since a
// scripted run does not produce them):
//   TODO(incremental): lines carry the offsets of the record that last defined the message; a
//   $patch that fills in a tool result later does not move them, so a session ingested while a
//   tool was still running keeps the call without its result. mg sees the record on a later run.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// Gemini is the Gemini CLI adapter.
type Gemini struct{}

func (Gemini) Kind() ir.SourceKind { return ir.Gemini }

func (Gemini) Discover(cfg *config.Config) []ir.SourceFile {
	var mains, subs []ir.SourceFile
	byID := map[string]string{} // session id → main file, to link subagents
	for _, root := range cfg.Dirs(ir.Gemini) {
		root = filepath.Clean(root)
		for _, f := range walkFiles(root, func(n string) bool {
			return strings.HasSuffix(n, ".jsonl") || (strings.HasPrefix(n, "session-") && strings.HasSuffix(n, ".json"))
		}, 4) {
			chats := inChats(f.path)
			if chats == "" {
				continue
			}
			head, err := readHead(f.path, 8192)
			if err != nil || !(Gemini{}).Sniff(f.path, head) {
				continue
			}
			sf := ir.SourceFile{Kind: ir.Gemini, Path: f.path, Key: f.path, Size: f.size, MtimeMs: f.mtimeMs}
			if filepath.Dir(f.path) == chats {
				var meta map[string]any
				_ = json.Unmarshal(firstLine(head), &meta)
				if id := str(meta, "sessionId"); id != "" {
					byID[id] = f.path
				}
				mains = append(mains, sf)
				continue
			}
			sf.ParentKey = filepath.Base(filepath.Dir(f.path)) // resolved below
			sf.AgentName = strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
			subs = append(subs, sf)
		}
	}
	out := mains
	if cfg.Subagents() {
		for _, sf := range subs {
			if parent, ok := byID[sf.ParentKey]; ok {
				sf.ParentKey = parent
				out = append(out, sf)
			}
		}
	}
	return out
}

// inChats returns the .../chats directory containing path, or "".
func inChats(path string) string {
	for d := filepath.Dir(path); d != filepath.Dir(d); d = filepath.Dir(d) {
		if filepath.Base(d) == "chats" {
			return d
		}
	}
	return ""
}

func (Gemini) Sniff(_ string, head []byte) bool {
	h := bytes.TrimSpace(head)
	if len(h) == 0 || h[0] != '{' {
		return false
	}
	if i := bytes.IndexByte(h, '\n'); i >= 0 && bytes.HasPrefix(h, []byte(`{"`)) {
		h = h[:i] // JSONL: the metadata line is the first line
	} else if len(h) > 2048 {
		h = h[:2048]
	}
	return bytes.Contains(h, []byte(`"sessionId"`)) && bytes.Contains(h, []byte(`"projectHash"`))
}

func (g Gemini) Open(f ir.SourceFile) (*ir.Session, error) {
	buf, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	return g.OpenBytes(f, buf, ScanOptions{})
}

type geminiMessage struct {
	obj        map[string]any
	start, end int64
	line       int
}

// geminiReplay applies every line of a session file and returns the final conversation.
type geminiReplay struct {
	meta     map[string]any
	order    []string
	messages map[string]*geminiMessage
	read     jsonlRead
}

func (r *geminiReplay) drop(id string) {
	delete(r.messages, id)
	for i, x := range r.order {
		if x == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

func (r *geminiReplay) put(o map[string]any, start, end int64, line int) {
	id := str(o, "id")
	if m, ok := r.messages[id]; ok {
		m.obj, m.start, m.end, m.line = o, start, end, line
		return
	}
	r.messages[id] = &geminiMessage{obj: o, start: start, end: end, line: line}
	r.order = append(r.order, id)
}

func (r *geminiReplay) patch(p map[string]any) {
	apply := func(pm map[string]any) {
		m := r.messages[str(pm, "id")]
		if m == nil {
			return
		}
		if c, ok := pm["content"]; ok {
			m.obj["content"] = c
		}
		for _, tp := range arr(pm["toolCalls"]) {
			tpm, _ := tp.(map[string]any)
			for _, tc := range arr(m.obj["toolCalls"]) {
				tcm, _ := tc.(map[string]any)
				if str(tcm, "id") == str(tpm, "id") {
					if res, ok := tpm["result"]; ok {
						tcm["result"] = res
					}
				}
			}
		}
	}
	if str(p, "id") != "" {
		apply(p)
	}
	for _, u := range arr(p["updates"]) {
		um, _ := u.(map[string]any)
		apply(um)
	}
	for _, id := range arr(p["removeIds"]) {
		if s, ok := id.(string); ok {
			r.drop(s)
		}
	}
	if ids := arr(p["orderIds"]); ids != nil {
		in := map[string]bool{}
		var ordered []string
		for _, id := range ids {
			if s, ok := id.(string); ok && r.messages[s] != nil && !in[s] {
				in[s] = true
				ordered = append(ordered, s)
			}
		}
		var rest []string
		for _, id := range r.order {
			if !in[id] {
				rest = append(rest, id)
			}
		}
		r.order = append(rest, ordered...)
	}
}

func replayGemini(buf []byte, legacy bool) *geminiReplay {
	r := &geminiReplay{meta: map[string]any{}, messages: map[string]*geminiMessage{}}
	if legacy {
		var whole map[string]any
		if json.Unmarshal(buf, &whole) != nil {
			return r
		}
		r.read = jsonlRead{size: int64(len(buf)), completeBytes: int64(len(buf))}
		for k, v := range whole {
			if k != "messages" {
				r.meta[k] = v
			}
		}
		for i, m := range arr(whole["messages"]) {
			if mm, ok := m.(map[string]any); ok && str(mm, "id") != "" {
				r.put(mm, 0, int64(len(buf)), i+1)
			}
		}
		return r
	}
	r.read = parseJSONL(buf)
	for _, rec := range r.read.records {
		o := rec.obj
		switch {
		case str(o, "$rewindTo") != "":
			id := str(o, "$rewindTo")
			if _, ok := r.messages[id]; !ok {
				r.order, r.messages = nil, map[string]*geminiMessage{}
				continue
			}
			for i, x := range r.order {
				if x == id {
					for _, gone := range r.order[i:] {
						delete(r.messages, gone)
					}
					r.order = r.order[:i]
					break
				}
			}
		case obj(o, "$patch") != nil:
			r.patch(obj(o, "$patch"))
		case obj(o, "$set") != nil:
			set := obj(o, "$set")
			if ms, ok := set["messages"].([]any); ok { // legacy checkpoint: replaces all
				r.order, r.messages = nil, map[string]*geminiMessage{}
				for _, m := range ms {
					if mm, ok := m.(map[string]any); ok && str(mm, "id") != "" {
						r.put(mm, rec.start, rec.end, rec.line)
					}
				}
			}
			for k, v := range set {
				if k != "messages" {
					r.meta[k] = v
				}
			}
		case str(o, "id") != "":
			r.put(o, rec.start, rec.end, rec.line)
		case str(o, "sessionId") != "" && str(o, "projectHash") != "":
			for k, v := range o {
				r.meta[k] = v
			}
		}
	}
	return r
}

// OpenBytes parses a session file already read. ScanOptions are not used yet.
func (Gemini) OpenBytes(f ir.SourceFile, buf []byte, _ ScanOptions) (*ir.Session, error) {
	r := replayGemini(buf, strings.HasSuffix(f.Path, ".json"))
	id := str(r.meta, "sessionId")
	if id == "" {
		return nil, nil
	}
	s := &ir.Session{
		Kind:          ir.Gemini,
		File:          f,
		SessionID:     id,
		Title:         strings.TrimSpace(str(r.meta, "summary")),
		StartedAt:     str(r.meta, "startTime"),
		CompleteBytes: r.read.completeBytes,
		BadLines:      r.read.badLines,
		Cwd:           geminiCwd(f.Path, r.meta),
	}
	subagent := str(r.meta, "kind") == "subagent"
	s.Render = func(from, to int64) []ir.Line {
		var lines []ir.Line
		tasked := false // a subagent's first user message is the task its parent gave it
		for _, id := range r.order {
			m := r.messages[id]
			if m.start < from || (to >= 0 && m.start >= to) {
				continue
			}
			ts := str(m.obj, "timestamp")
			push := func(role ir.Role, text string, turnStart bool, success *bool) {
				if !util.IsSpaceOnly(text) {
					lines = append(lines, ir.Line{EntryID: id, TS: ts, Role: role, Text: util.Redact(text), TurnStart: turnStart, Start: m.start, End: m.end, Success: success})
				}
			}
			if subagent && str(m.obj, "type") == "user" {
				// The first user message of a subagent file is the task its parent delegated; the later
				// ones are the loop's own nudges ("You have stopped calling tools…").
				if text := partsText(contentParts(m.obj["content"])); !tasked && strings.TrimSpace(text) != "" {
					tasked = true
					push(ir.RoleTask, "TASK for subagent: "+util.TruncateMiddle(text, 3000), true, nil)
				}
				continue
			}
			renderGeminiMessage(m.obj, push)
		}
		return lines
	}
	return s, nil
}

// geminiCwd is the project's working directory: `.project_root` next to the chats directory,
// else the first workspace directory a subagent recorded.
func geminiCwd(path string, meta map[string]any) string {
	if chats := inChats(path); chats != "" {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(chats), ".project_root")); err == nil {
			if p := strings.TrimSpace(string(b)); p != "" {
				return p
			}
		}
	}
	if d := arr(meta["directories"]); len(d) > 0 {
		if s, ok := d[0].(string); ok {
			return s
		}
	}
	return ""
}

// geminiOwnResult returns the parts of a tool call's result that answer that call. After a parallel
// batch the CLI stores the whole batch's responses (every functionResponse of the turn) in the
// `result` of each call, so the parts are matched by call id, then by tool name.
func geminiOwnResult(tc map[string]any) []any {
	parts := contentParts(tc["result"])
	var byID, byName []any
	for _, p := range parts {
		fr := obj(mapOf(p), "functionResponse")
		if fr == nil {
			continue
		}
		if id := str(tc, "id"); id != "" && str(fr, "id") == id {
			byID = append(byID, p)
		} else if str(fr, "name") == str(tc, "name") {
			byName = append(byName, p)
		}
	}
	switch {
	case len(byID) > 0:
		return byID
	case len(byName) == 1 || (len(byName) > 0 && len(parts) == len(byName)):
		return byName[:1]
	}
	return parts
}

var (
	geminiContextRe = regexp.MustCompile(`(?s)^\s*<untrusted_context>\n?(.*?)\n?</untrusted_context>\s*$`)
	geminiExitRe    = regexp.MustCompile(`(?m)^Exit Code: (\S+)\n?`)
	geminiNoiseRe   = regexp.MustCompile(`(?m)^(Process Group PGID|Signal|Background PIDs): .*\n?`)
)

// geminiToolOutput unwraps what the shell tool reports (checked on 0.66.0): the output inside
// <untrusted_context>, an "Exit Code: N" line, and process bookkeeping. A non-zero exit code makes
// the call a failure although its status is "success". Other results are returned unchanged.
func geminiToolOutput(text string) (out string, failed bool) {
	m := geminiContextRe.FindStringSubmatch(text)
	if m == nil {
		return text, false
	}
	body := m[1]
	if e := geminiExitRe.FindStringSubmatch(body); e != nil && e[1] != "0" {
		failed = true
		body = geminiExitRe.ReplaceAllString(body, "")
		body = strings.TrimSpace(geminiNoiseRe.ReplaceAllString(body, "")) + " (exit " + e[1] + ")"
	} else {
		body = geminiExitRe.ReplaceAllString(body, "")
		body = strings.TrimSpace(geminiNoiseRe.ReplaceAllString(body, ""))
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "Output: ")), failed
}

// A slash command typed into the prompt ("/help", "/memory show") is not part of the story.
var slashCommandRe = regexp.MustCompile(`^/[A-Za-z][\w:-]*(\s|$)`)

func ignoredGeminiUser(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.HasPrefix(t, "?") || slashCommandRe.MatchString(t) ||
		strings.HasPrefix(t, "<session_context>") || strings.HasPrefix(t, "<hook_context>")
}

func renderGeminiMessage(m map[string]any, push pushFunc) {
	switch str(m, "type") {
	case "user":
		if text := partsText(contentParts(m["content"])); !ignoredGeminiUser(text) {
			pushUser(push, text)
		}
	case "gemini":
		pushAssistantText(push, partsText(contentParts(m["content"])))
		for _, tc := range arr(m["toolCalls"]) {
			tcm, _ := tc.(map[string]any)
			name := str(tcm, "name")
			pushToolCall(push, map[string]string{}, str(tcm, "id"), name, tcm["args"])
			status := str(tcm, "status")
			text, failed := "", status == "error" || status == "cancelled"
			for _, p := range geminiOwnResult(tcm) {
				pm, _ := p.(map[string]any)
				if fr := obj(pm, "functionResponse"); fr != nil {
					t, isErr := functionResponseText(fr)
					text, failed = t, failed || isErr
				} else if t, ok := pm["text"].(string); ok {
					text = t
				}
			}
			switch {
			case tcm["result"] != nil || failed:
				if text == "" && status == "cancelled" {
					text = "cancelled"
				}
				out, bad := geminiToolOutput(text)
				pushToolResult(push, name, out, failed || bad)
			}
		}
	case "error":
		if msg := partsText(contentParts(m["content"])); strings.TrimSpace(msg) != "" {
			push(ir.RoleNote, "  [assistant error: "+util.Truncate(msg, 300)+"]", false, nil)
		}
	}
	// info, warning: messages of the CLI itself
}
