package sources

// omp / pi adapter — the reference implementation, verified against omp 18.13.2
// (src/session/session-entries.ts in @oh-my-pi/pi-coding-agent) and covered by fixtures that
// omp's own SessionManager generated (test/fixtures/gen-fixtures.ts).
//
// Layout:        ~/.omp/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl
// Subagents:     .../<timestamp>_<uuid>/<AgentName>.jsonl  (directory named like the parent)
// Advisor runs:  .../<timestamp>_<uuid>/__advisor.<n>.jsonl
//
// File format (v3): an optional fixed-width `title` slot line, then a `session` header, then
// entries linked into a tree by id/parentId. Files are append-only, so file order is
// chronological; a branch shows up as an entry whose parentId is not the previous leaf. omp may
// write off-branch entries carrying `activeLeafId` (late usage, background results), which
// restore the leaf instead of moving it. Project identity comes from the header's cwd — never
// reverse the encoded directory name.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// OMP is the omp / pi adapter.
type OMP struct{}

func (OMP) Kind() ir.SourceKind { return ir.OMP }

func (OMP) Discover(cfg *config.Config) []ir.SourceFile {
	return ListOmpFiles(cfg.Dirs(ir.OMP), cfg.Subagents(), cfg.Advisor())
}

func (OMP) Sniff(_ string, head []byte) bool {
	var o map[string]any
	if json.Unmarshal(firstLine(head), &o) == nil {
		t := str(o, "type")
		return t == "title" || t == "session"
	}
	return regexp.MustCompile(`^\{"type":"(title|session)"`).Match(head)
}

func (OMP) Open(f ir.SourceFile) (*ir.Session, error) {
	p, err := ParseOmp(f.Path)
	if err != nil {
		return nil, err
	}
	return ompSession(f, p), nil
}

// OpenBytes parses a session file already read, honouring opt (see ScanOptions).
func (OMP) OpenBytes(f ir.SourceFile, raw []byte, opt ScanOptions) (*ir.Session, error) {
	p := parseOmpBytes(f.Path, raw, opt.Keep)
	p.raw = opt.Raw
	return ompSession(f, p), nil
}

func ompSession(f ir.SourceFile, p *OmpSession) *ir.Session {
	if p.Header == nil {
		return nil
	}
	s := &ir.Session{
		Kind:          ir.OMP,
		File:          f,
		SessionID:     str(p.Header, "id"),
		Cwd:           str(p.Header, "cwd"),
		Title:         p.Title,
		StartedAt:     str(p.Header, "timestamp"),
		CompleteBytes: p.CompleteBytes,
		BadLines:      p.BadLines,
		Render:        func(from, to int64) []ir.Line { return RenderOmp(p, from, to) },
	}
	return s
}

// OmpEntry is one tree entry of an omp session.
type OmpEntry struct {
	Obj        map[string]any
	ID         string
	ParentID   string // "" = root
	HasParent  bool
	Start, End int64
	// Elided entries were only skimmed (ScanOptions.Keep): Obj holds just the fields the tree
	// and the renderer's bookkeeping need (type, id, parentId, activeLeafId, timestamp, role).
	Elided bool
}

func (e *OmpEntry) typ() string { return str(e.Obj, "type") }

// OmpSession is a parsed omp session file.
type OmpSession struct {
	Path          string
	Header        map[string]any
	Title         string
	Entries       []*OmpEntry
	CompleteBytes int64
	Size          int64
	BadLines      int
	raw           bool // render without redaction (ScanOptions.Raw)
}

// ParseOmp reads an omp/pi session file. A partially written last line is ignored; v1 sessions
// without ids are chained linearly.
func ParseOmp(path string) (*OmpSession, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseOmpBytes(path, buf, nil), nil
}

// ompRendered are the entry types RenderOmp turns into lines; every other type only matters
// for the tree, so a search never decodes it in full.
var ompRendered = map[string]bool{"title": true, "session": true, "title_change": true, "message": true,
	"session_init": true, "branch_summary": true, "reset_boundary": true, "mode_change": true}

// skim reads the fields of an entry that structure the transcript (type, id, parentId,
// activeLeafId, timestamp, and the message's role and toolName) without decoding the rest, and
// returns them as the map a full decode would hold for them. ok is false when the line must be
// decoded in full: not a plain JSON object, a type that renders lines that open or structure a
// turn, a user message, or a tool result whose tool name comes from elsewhere. roles are those of
// the lines a message may render, for ScanOptions.Keep to decide on; nil for entries that render
// nothing.
func skim(line []byte) (o map[string]any, ok bool, roles []ir.Role, message []byte) {
	o = map[string]any{}
	var role, tool any
	ok = scanObject(line, func(key string, val []byte) bool {
		switch key {
		case "type", "id", "parentId", "activeLeafId", "timestamp":
			v, ok := scalar(val)
			o[key] = v
			return ok
		case "message":
			if len(val) == 0 || val[0] != '{' {
				return false
			}
			message = val
			return scanObject(val, func(k string, v []byte) bool {
				var ok bool
				switch k {
				case "role":
					role, ok = scalar(v)
					return ok
				case "toolName":
					tool, ok = scalar(v)
					return ok
				}
				return true
			})
		}
		return true
	})
	if !ok {
		return nil, false, nil, nil
	}
	t := str(o, "type")
	if !ompRendered[t] {
		return o, true, nil, nil
	}
	r, _ := role.(string)
	if t != "message" || r == "" || r == "user" {
		return nil, false, nil, nil
	}
	if tn, _ := tool.(string); r == "toolResult" && tn == "" {
		return nil, false, nil, nil
	}
	o["message"] = map[string]any{"role": r}
	return o, true, ompMessageRoles[r], message
}

// ompMessage holds the message fields RenderOmp reads. Decoding into it skips everything else
// (thinking blocks, usage, tool result details), the bulk of an agent message.
type ompMessage struct {
	Role, StopReason, ErrorMessage, ToolName, ToolCallID any
	IsError, Command, Code, Output, ExitCode, Cancelled  any
	Content                                              json.RawMessage
}

type ompBlock struct {
	Type, Text, ID, Name, Arguments any
}

// decodeOmpMessage decodes a message the way a full decode would present it to RenderOmp: the
// same map, minus the fields it never reads. ok is false when the message is not valid JSON.
func decodeOmpMessage(raw []byte) (map[string]any, bool) {
	var m ompMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	out := map[string]any{}
	for k, v := range map[string]any{"role": m.Role, "stopReason": m.StopReason, "errorMessage": m.ErrorMessage,
		"toolName": m.ToolName, "toolCallId": m.ToolCallID, "isError": m.IsError, "command": m.Command,
		"code": m.Code, "output": m.Output, "exitCode": m.ExitCode, "cancelled": m.Cancelled} {
		if v != nil {
			out[k] = v
		}
	}
	c := bytes.TrimSpace(m.Content)
	switch {
	case len(c) == 0:
	case c[0] == '[':
		var items []json.RawMessage
		if json.Unmarshal(c, &items) != nil {
			return nil, false
		}
		blocks := make([]any, len(items))
		for i, it := range items {
			if it = bytes.TrimSpace(it); len(it) == 0 || it[0] != '{' {
				var x any
				if json.Unmarshal(it, &x) != nil {
					return nil, false
				}
				blocks[i] = x
				continue
			}
			var b ompBlock
			if json.Unmarshal(it, &b) != nil {
				return nil, false
			}
			bm := map[string]any{}
			for k, v := range map[string]any{"type": b.Type, "text": b.Text, "id": b.ID, "name": b.Name, "arguments": b.Arguments} {
				if v != nil {
					bm[k] = v
				}
			}
			blocks[i] = bm
		}
		out["content"] = blocks
	default:
		var x any
		if json.Unmarshal(c, &x) != nil {
			return nil, false
		}
		out["content"] = x
	}
	return out, true
}

// ompMessageRoles are the roles of the lines RenderOmp renders for each message role; a message
// role missing here renders nothing.
var ompMessageRoles = map[string][]ir.Role{
	"assistant":       {ir.RoleAssistant, ir.RoleTool, ir.RoleNote},
	"toolResult":      {ir.RoleResult},
	"bashExecution":   {ir.RoleShell},
	"pythonExecution": {ir.RoleShell},
}

// scalar decodes a JSON string, null, boolean or number; ok is false for objects and arrays.
func scalar(v []byte) (any, bool) {
	if len(v) == 0 || v[0] == '{' || v[0] == '[' {
		return nil, false
	}
	if v[0] == '"' && bytes.IndexByte(v, '\\') < 0 {
		return string(v[1 : len(v)-1]), true
	}
	var x any
	return x, json.Unmarshal(v, &x) == nil
}

// scanObject walks the members of the JSON object at the start of b (which must hold nothing
// else), handing each key and raw value to fn, and reports whether b is well-formed and fn
// returned true throughout. String contents are skipped with bytes.IndexByte, so a skim costs a
// fraction of a decode. It checks structure only: number syntax and control characters inside
// strings are not validated.
func scanObject(b []byte, fn func(key string, val []byte) bool) bool {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return false
	}
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == '}' {
		return skipSpace(b, i+1) == len(b)
	}
	for {
		if i >= len(b) || b[i] != '"' {
			return false
		}
		end := skipString(b, i)
		if end < 0 {
			return false
		}
		key := b[i+1 : end-1]
		i = skipSpace(b, end)
		if i >= len(b) || b[i] != ':' {
			return false
		}
		i = skipSpace(b, i+1)
		vend := skipValue(b, i)
		if vend < 0 {
			return false
		}
		k := string(key)
		if bytes.IndexByte(key, '\\') >= 0 { // rare: decode the escapes
			var dk string
			if json.Unmarshal(append([]byte{'"'}, append(key, '"')...), &dk) != nil {
				return false
			}
			k = dk
		}
		if !fn(k, b[i:vend]) {
			return false
		}
		i = skipSpace(b, vend)
		if i >= len(b) {
			return false
		}
		switch b[i] {
		case ',':
			i = skipSpace(b, i+1)
		case '}':
			return skipSpace(b, i+1) == len(b)
		default:
			return false
		}
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// skipString returns the offset after the string starting at b[i] (a quote), or -1.
func skipString(b []byte, i int) int {
	i++
	for {
		j := bytes.IndexByte(b[i:], '"')
		if j < 0 {
			return -1
		}
		j += i
		// The quote is escaped if an odd number of backslashes precede it.
		n := 0
		for k := j - 1; k >= i && b[k] == '\\'; k-- {
			n++
		}
		if n%2 == 0 {
			return j + 1
		}
		i = j + 1
	}
}

// skipValue returns the offset after the JSON value starting at b[i], or -1.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				if i = skipString(b, i); i < 0 {
					return -1
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return -1
	default:
		start := i
		for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' && b[i] != ' ' && b[i] != '\t' && b[i] != '\r' && b[i] != '\n' {
			i++
		}
		if i == start {
			return -1
		}
		return i
	}
}

// parseOmpBytes parses a session file's bytes. With keep set, entries that render no turn
// opening are skimmed unless keep accepts them (see ScanOptions.Keep); everything up to the first
// user message or task is always decoded in full, so the turns before it come out unchanged.
func parseOmpBytes(path string, buf []byte, keep func([]byte, []ir.Role) bool) *OmpSession {
	p := &OmpSession{Path: path, Size: int64(len(buf))}
	opened := false // a user message, task or /clear was seen: later records may be skimmed
	lineNo := 0
	lastID := ""
	hasLast := false
	accept := func(o map[string]any, start, end int64) {
		lineNo++
		if o == nil {
			return
		}
		switch t := str(o, "type"); {
		case t == "title":
			if s := strings.TrimSpace(str(o, "title")); s != "" {
				p.Title = s
			}
			return
		case t == "session" && p.Header == nil:
			p.Header = o
			if p.Title == "" {
				p.Title = str(o, "title")
			}
			return
		case t == "title_change":
			if s, ok := o["title"].(string); ok {
				p.Title = s
			}
		}
		e := &OmpEntry{Obj: o, Start: start, End: end}
		if id, ok := o["id"].(string); ok {
			e.ID = id
			switch pid := o["parentId"].(type) {
			case string:
				e.ParentID, e.HasParent = pid, true
			case nil:
				if _, present := o["parentId"]; !present && hasLast {
					e.ParentID, e.HasParent = lastID, true
				}
			}
		} else {
			// v1 sessions have no ids: chain them linearly.
			e.ID = fmt.Sprintf("L%d", lineNo)
			e.ParentID, e.HasParent = lastID, hasLast
		}
		p.Entries = append(p.Entries, e)
		lastID, hasLast = e.ID, true
	}

	pos := 0
	for pos < len(buf) {
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl == -1 {
			// Unterminated last line: accept only if it is complete JSON.
			if rest := bytes.TrimSpace(buf[pos:]); len(rest) > 0 {
				var o map[string]any
				if json.Unmarshal(rest, &o) == nil {
					accept(o, int64(pos), int64(len(buf)))
					p.CompleteBytes = int64(len(buf))
				}
			}
			break
		}
		start := pos
		line := bytes.TrimSpace(buf[pos : pos+nl])
		pos += nl + 1
		p.CompleteBytes = int64(pos)
		if len(line) == 0 {
			continue
		}
		if keep != nil && opened {
			o, ok, roles, message := skim(line)
			if ok && (roles == nil || !keep(line, roles)) {
				n := len(p.Entries)
				accept(o, int64(start), int64(pos))
				if len(p.Entries) > n {
					p.Entries[n].Elided = true
				}
				continue
			}
			if ok {
				// A message that may hold a match: decode what the renderer reads.
				if m, ok := decodeOmpMessage(message); ok {
					o["message"] = m
					accept(o, int64(start), int64(pos))
					continue
				}
			}
		}
		var o map[string]any
		if json.Unmarshal(line, &o) != nil {
			p.BadLines++
			continue
		}
		accept(o, int64(start), int64(pos))
		if !opened {
			switch str(o, "type") {
			case "message":
				opened = str(obj(o, "message"), "role") == "user"
			case "session_init", "reset_boundary":
				opened = true
			}
		}
	}
	if keep != nil {
		p.decodeAnonymousResults(buf)
	}
	return p
}

// decodeAnonymousResults handles a tool result without a tool name: the renderer names it after
// the tool call, which a skimmed assistant entry no longer carries, so then every skimmed
// message is decoded after all.
func (p *OmpSession) decodeAnonymousResults(buf []byte) {
	anonymous := false
	for _, e := range p.Entries {
		if !e.Elided && e.typ() == "message" {
			m := obj(e.Obj, "message")
			if str(m, "role") == "toolResult" && str(m, "toolName") == "" {
				anonymous = true
				break
			}
		}
	}
	if !anonymous {
		return
	}
	for _, e := range p.Entries {
		if !e.Elided {
			continue
		}
		var o map[string]any
		if json.Unmarshal(bytes.TrimSpace(buf[e.Start:e.End]), &o) == nil {
			e.Obj, e.Elided = o, false
		}
	}
}

// contentText joins the text blocks of a message content (string or block array).
func contentText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	for _, b := range arr(content) {
		bm, _ := b.(map[string]any)
		switch str(bm, "type") {
		case "text":
			if t, ok := bm["text"].(string); ok {
				parts = append(parts, t)
			}
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

var argKeys = []string{"command", "cmd", "code", "path", "file_path", "filePath", "paths", "file", "pattern",
	"query", "url", "glob", "agent", "description", "task", "prompt"}
var longArgKeys = map[string]bool{"command": true, "cmd": true, "code": true, "task": true, "prompt": true, "description": true}
var spaceRe = regexp.MustCompile(`\s+`)

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// SummarizeArgs renders the interesting tool-call arguments on one line.
func SummarizeArgs(args any) string {
	if args == nil {
		return ""
	}
	a, ok := args.(map[string]any)
	if !ok {
		if s, ok := args.(string); ok {
			return util.Truncate(s, 200)
		}
		return util.Truncate(jsonText(args), 200)
	}
	var parts []string
	known := map[string]bool{}
	for _, k := range argKeys {
		known[k] = true
		v, ok := a[k]
		if !ok || v == nil || v == "" {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			s = jsonText(v)
		}
		max := 160
		if longArgKeys[k] {
			max = 400
		}
		parts = append(parts, k+"="+util.Truncate(spaceRe.ReplaceAllString(s, " "), max))
	}
	if len(parts) == 0 {
		return util.Truncate(jsonText(a), 300)
	}
	var rest []string
	for k := range a {
		if !known[k] {
			rest = append(rest, k)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest) // Go maps are unordered; JS kept insertion order
		if len(rest) > 6 {
			rest = rest[:6]
		}
		parts = append(parts, "(+"+strings.Join(rest, ",")+")")
	}
	return strings.Join(parts, " ")
}

var shellToolRe = regexp.MustCompile(`(?i)bash|shell|exec|run|python|eval|test`)

// RenderOmp renders entries into compact transcript lines. Only entries starting in [from, to)
// are emitted (to < 0: no upper bound), but the whole file is walked so branch detection stays
// correct for incremental reads.
func RenderOmp(p *OmpSession, from, to int64) []ir.Line {
	var lines []ir.Line
	cursor, hasCursor := "", false
	toolNames := map[string]string{}

	for _, e := range p.Entries {
		emit := e.Start >= from && (to < 0 || e.Start < to)
		ts := str(e.Obj, "timestamp")
		push := func(role ir.Role, text string, turnStart bool, success *bool) {
			if emit && !util.IsSpaceOnly(text) {
				if !p.raw {
					text = util.Redact(text)
				}
				lines = append(lines, ir.Line{EntryID: e.ID, TS: ts, Role: role, Text: text, TurnStart: turnStart, Start: e.Start, End: e.End, Success: success})
			}
		}

		// --- branch bookkeeping ---------------------------------------------
		_, offBranch := e.Obj["activeLeafId"]
		if !offBranch && hasCursor && e.typ() != "label" {
			if !e.HasParent {
				push(ir.RoleNote, "[context reset: a new root was started; earlier conversation is no longer in context]", true, nil)
			} else if e.ParentID != cursor {
				push(ir.RoleNote, fmt.Sprintf("[branch: rewound to earlier point #%s; everything after that point on the previous path was abandoned]", e.ParentID), true, nil)
			}
		}

		// --- content ----------------------------------------------------------
		switch {
		case e.Elided:
			if emit && e.typ() == "message" && ompMessageRoles[str(obj(e.Obj, "message"), "role")] != nil {
				role := ir.RoleAssistant
				switch str(obj(e.Obj, "message"), "role") {
				case "toolResult":
					role = ir.RoleResult
				case "bashExecution", "pythonExecution":
					role = ir.RoleShell
				}
				lines = append(lines, ir.Line{EntryID: e.ID, TS: ts, Role: role, Start: e.Start, End: e.End, Elided: true})
			}
		default:
			renderOmpEntry(e, push, toolNames)
		}

		if offBranch {
			// a null activeLeafId clears the cursor, like the TS original
			cursor, hasCursor = e.Obj["activeLeafId"].(string)
		} else {
			cursor, hasCursor = e.ID, true
		}
	}
	return lines
}

// renderOmpEntry renders the content of one decoded entry.
func renderOmpEntry(e *OmpEntry, push func(role ir.Role, text string, turnStart bool, success *bool), toolNames map[string]string) {
	switch e.typ() {
	case "message":
		m := obj(e.Obj, "message")
		switch str(m, "role") {
		case "user":
			if t := strings.TrimSpace(contentText(m["content"])); t != "" {
				push(ir.RoleUser, "USER: "+util.TruncateMiddle(t, 4000), true, nil)
			}
		case "assistant":
			for _, b := range arr(m["content"]) {
				bm, _ := b.(map[string]any)
				switch str(bm, "type") {
				case "text":
					if t := strings.TrimSpace(str(bm, "text")); t != "" {
						push(ir.RoleAssistant, "ASSISTANT: "+util.TruncateMiddle(t, 2500), false, nil)
					}
				case "toolCall":
					if id := str(bm, "id"); id != "" {
						toolNames[id] = str(bm, "name")
					}
					push(ir.RoleTool, fmt.Sprintf("  → %s(%s)", str(bm, "name"), SummarizeArgs(bm["arguments"])), false, nil)
				}
			}
			switch str(m, "stopReason") {
			case "aborted":
				push(ir.RoleNote, "  [assistant turn aborted by the user]", false, nil)
			case "error":
				msg := str(m, "errorMessage")
				if msg == "" {
					msg = "unknown"
				}
				push(ir.RoleNote, fmt.Sprintf("  [assistant error: %s]", util.Truncate(msg, 300)), false, nil)
			}
		case "toolResult":
			name := str(m, "toolName")
			if name == "" {
				name = toolNames[str(m, "toolCallId")]
			}
			if name == "" {
				name = "tool"
			}
			t := strings.TrimSpace(contentText(m["content"]))
			switch {
			case truthy(m["isError"]):
				push(ir.RoleResult, fmt.Sprintf("  ✗ %s failed: %s", name, util.TruncateMiddle(t, 700)), false, ir.Bool(false))
			case shellToolRe.MatchString(name):
				push(ir.RoleResult, fmt.Sprintf("  ✓ %s: %s", name, util.Tail(t, 300)), false, ir.Bool(true))
			default:
				suffix := ""
				if t != "" {
					suffix = ": " + util.Truncate(spaceRe.ReplaceAllString(t, " "), 120)
				}
				push(ir.RoleResult, fmt.Sprintf("  ✓ %s%s", name, suffix), false, ir.Bool(true))
			}
		case "bashExecution":
			push(ir.RoleShell, fmt.Sprintf("USER SHELL $ %s (exit %s)\n%s", util.Truncate(str(m, "command"), 400), exitCode(m), util.Tail(fmt.Sprint(orEmpty(m["output"])), 400)), false, nil)
		case "pythonExecution":
			push(ir.RoleShell, fmt.Sprintf("USER PYTHON >>> %s (exit %s)\n%s", util.Truncate(str(m, "code"), 400), exitCode(m), util.Tail(fmt.Sprint(orEmpty(m["output"])), 400)), false, nil)
		default:
			// system / developer / custom / hookMessage / summaries: not part of the story
		}
	case "session_init":
		who := "subagent"
		if a := str(e.Obj, "agent"); a != "" {
			who = fmt.Sprintf("subagent %q", a)
		}
		if task := str(e.Obj, "task"); strings.TrimSpace(task) != "" {
			push(ir.RoleTask, fmt.Sprintf("TASK for %s: %s", who, util.TruncateMiddle(task, 3000)), true, nil)
		}
	case "branch_summary":
		if s := e.Obj["summary"]; truthy(s) {
			push(ir.RoleNote, fmt.Sprintf("[summary of the abandoned branch: %s]", util.TruncateMiddle(fmt.Sprint(s), 1200)), false, nil)
		}
	case "reset_boundary":
		push(ir.RoleNote, "[user ran /clear — a new topic likely starts here]", true, nil)
	case "mode_change":
		if mode := str(e.Obj, "mode"); mode != "" && mode != "none" {
			push(ir.RoleNote, fmt.Sprintf("[mode: %s]", mode), false, nil)
		}
	default:
		// compaction, custom, custom_message, label, model_change, usage, ttsr_injection, ...
	}
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func exitCode(m map[string]any) string {
	switch v := m["exitCode"].(type) {
	case float64:
		return fmt.Sprintf("%d", int(v))
	case string:
		return v
	}
	if truthy(m["cancelled"]) {
		return "cancelled"
	}
	return "?"
}

// ListOmpFiles finds omp/pi session files: main sessions plus subagent and advisor files in the
// directory named after their parent session.
func ListOmpFiles(roots []string, includeSubagents, includeAdvisor bool) []ir.SourceFile {
	var out []ir.SourceFile
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, d := range entries {
			p := filepath.Join(dir, d.Name())
			st, err := os.Stat(p)
			if err != nil {
				continue
			}
			if st.IsDir() {
				if depth < 3 {
					walk(p, depth+1)
				}
				continue
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			sibling := filepath.Dir(p) + ".jsonl"
			_, serr := os.Stat(sibling)
			isChild := depth >= 2 && serr == nil
			isAdvisor := strings.HasPrefix(name, "__advisor")
			if isChild && !includeSubagents && !isAdvisor {
				continue
			}
			if isAdvisor && !includeAdvisor {
				continue
			}
			f := ir.SourceFile{Kind: ir.OMP, Path: p, Key: p, Size: st.Size(), MtimeMs: float64(st.ModTime().UnixNano()) / 1e6, IsAdvisor: isAdvisor}
			if isChild {
				f.ParentKey = sibling
				f.AgentName = strings.TrimSuffix(name, ".jsonl")
			}
			out = append(out, f)
		}
	}
	for _, r := range roots {
		if _, err := os.Stat(r); err == nil {
			walk(r, 0)
		}
	}
	// parents before children, older before newer
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := out[i].ParentKey != "", out[j].ParentKey != ""
		if ci != cj {
			return !ci
		}
		return out[i].MtimeMs < out[j].MtimeMs
	})
	return out
}
