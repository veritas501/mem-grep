package sources

// Qwen Code adapter — read from the source of QwenLM/qwen-code (packages/core/src/services/
// chatRecordingService.ts and agents/agent-transcript.ts, main as of 2026-10-28) and checked against
// sessions Qwen Code 0.26.3 wrote itself (test/fixtures/qwen, made by gen.sh against a scripted mock
// model).
//
// Layout:        ~/.qwen/projects/<cwd with every non-alphanumeric character → "-">/chats/<sessionId>.jsonl
//                (next to it <sessionId>.runtime.json, a live-status sidecar to ignore)
// Subagents:     <project dir>/subagents/<parent sessionId>/agent-<id>.jsonl, ChatRecord-shaped with
//                isSidechain, plus agent-<id>.meta.json {agentType, description, …}
//
// File format (JSONL, append-only): ChatRecord {uuid, parentUuid, sessionId, timestamp, type, cwd,
// version, gitBranch?, message?: Content, model?, usageMetadata?, toolCallResult?, subtype?,
// systemPayload?, provenance?, …}. Like Claude Code the records form a tree through
// uuid / parentUuid; unlike it `message` is a Google GenAI Content {role, parts[]} (see parts.go).
//   type user         message.parts text; subtype mid_turn_user_message is a follow-up inside a turn
//   type assistant    parts: text, {text, thought: true}, {functionCall}
//   type tool_result  parts: {functionResponse}; toolCallResult.status
//   type system       bookkeeping, never rendered: slash_command, chat_compression, ui_telemetry,
//                     custom_title (systemPayload.customTitle), rewind, notification, …
// A user record with another subtype (notification, goal_runtime, agent_message, …) was written by
// the CLI or a workspace agent, not typed.
//
// What the real files showed: user records carry provenance "real_user"; every turn is followed by
// ui_telemetry system records (ignored); the shell tool reports "Command: … Directory: … Output: …
// Error: … Exit Code: N Signal: …" and a failing command comes back as response.error with status
// "error"; a headless 0.26.3 run offers no subagent tool, so subagent files (the `subagents/`
// layout above, from the source) have not been seen.
//
// Known gaps (qwen_test.go covers notifications and mid-turn messages on small records):
//   TODO(subagents): the layout is read from the source only.
//   TODO(rewind): `system`/`rewind` records and chat_compression checkpoints change which records
//   are active; records are rendered in file order, as if the conversation were linear.

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

// Qwen is the Qwen Code adapter.
type Qwen struct{}

func (Qwen) Kind() ir.SourceKind { return ir.Qwen }

func (Qwen) Discover(cfg *config.Config) []ir.SourceFile {
	var out []ir.SourceFile
	for _, root := range cfg.Dirs(ir.Qwen) {
		root = filepath.Clean(root)
		for _, f := range walkFiles(root, func(n string) bool { return strings.HasSuffix(n, ".jsonl") }, 4) {
			rel, err := filepath.Rel(root, f.path)
			if err != nil {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			sf := ir.SourceFile{Kind: ir.Qwen, Path: f.path, Key: f.path, Size: f.size, MtimeMs: f.mtimeMs}
			switch {
			case len(parts) == 3 && parts[1] == "chats": // <project>/chats/<id>.jsonl
			case len(parts) == 4 && parts[1] == "subagents" && strings.HasPrefix(parts[3], "agent-"):
				if !cfg.Subagents() {
					continue
				}
				sf.ParentKey = filepath.Join(root, parts[0], "chats", parts[2]+".jsonl")
				sf.AgentName = qwenAgentName(f.path)
			default:
				continue
			}
			if head, err := readHead(f.path, 16384); err != nil || !(Qwen{}).Sniff(f.path, head) {
				continue
			}
			out = append(out, sf)
		}
	}
	return out
}

// qwenAgentName reads the agent type from the sidecar agent-<id>.meta.json.
func qwenAgentName(jsonl string) string {
	name := strings.TrimSuffix(filepath.Base(jsonl), ".jsonl")
	if b, err := os.ReadFile(strings.TrimSuffix(jsonl, ".jsonl") + ".meta.json"); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil && str(m, "agentType") != "" {
			return str(m, "agentType")
		}
	}
	return name
}

// Sniff tells Qwen Code records from Claude Code's (same uuid / parentUuid / sessionId / cwd
// fields): Qwen's `message` holds GenAI `parts`, it has `tool_result` records and a `systemPayload`.
func (Qwen) Sniff(_ string, head []byte) bool {
	return qwenShaped(head)
}

func qwenShaped(head []byte) bool {
	for i, l := range strings.Split(string(head), "\n") {
		if i >= 20 {
			break
		}
		var o map[string]any
		if json.Unmarshal([]byte(l), &o) != nil || str(o, "sessionId") == "" || str(o, "uuid") == "" {
			continue
		}
		if _, ok := obj(o, "message")["parts"]; ok || str(o, "type") == "tool_result" || o["systemPayload"] != nil || o["provenance"] != nil {
			return true
		}
	}
	return false
}

func (q Qwen) Open(f ir.SourceFile) (*ir.Session, error) {
	buf, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	return q.OpenBytes(f, buf, ScanOptions{})
}

// OpenBytes parses a transcript already read. ScanOptions are not used yet.
func (Qwen) OpenBytes(f ir.SourceFile, buf []byte, _ ScanOptions) (*ir.Session, error) {
	read := parseJSONL(buf)
	s := &ir.Session{Kind: ir.Qwen, File: f, CompleteBytes: read.completeBytes, BadLines: read.badLines}
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
		if str(o, "type") == "system" && str(o, "subtype") == "custom_title" {
			if t := strings.TrimSpace(str(obj(o, "systemPayload"), "customTitle")); t != "" {
				s.Title = t
			}
		}
	}
	if s.SessionID == "" {
		return nil, nil
	}
	s.Render = func(from, to int64) []ir.Line {
		var lines []ir.Line
		toolNames := map[string]string{}
		for _, r := range read.records {
			o := r.obj
			emit := r.start >= from && (to < 0 || r.start < to)
			id := str(o, "uuid")
			ts := str(o, "timestamp")
			push := func(role ir.Role, text string, turnStart bool, success *bool) {
				if emit && !util.IsSpaceOnly(text) {
					lines = append(lines, ir.Line{EntryID: id, TS: ts, Role: role, Text: util.Redact(text), TurnStart: turnStart, Start: r.start, End: r.end, Success: success})
				}
			}
			parts := contentParts(obj(o, "message")["parts"])
			switch str(o, "type") {
			case "user":
				switch str(o, "subtype") {
				case "":
					pushUser(push, partsText(parts))
				case "mid_turn_user_message":
					if t := strings.TrimSpace(partsText(parts)); t != "" {
						push(ir.RoleUser, "USER: "+util.TruncateMiddle(t, 4000), false, nil)
					}
				}
				// every other subtype was written by the CLI or an agent, not typed
			case "assistant":
				renderQwenParts(parts, push, toolNames, o)
			case "tool_result":
				renderQwenParts(parts, push, toolNames, o)
			}
		}
		return lines
	}
	return s, nil
}

// qwenShellRe matches the report Qwen Code's shell tool returns: the command, directory, output,
// error text, exit code and signal, one per line (checked on 0.26.3).
var qwenShellRe = regexp.MustCompile(`(?s)^Command: .*?\nDirectory: .*?\nOutput: (.*?)\nError: (.*?)\nExit Code: (\S+)\nSignal: `)

// qwenShellText reduces a shell report to what happened: the output, or the error, and a non-zero
// exit code. Anything that is not such a report is returned unchanged.
func qwenShellText(text string) (out string, failed bool) {
	m := qwenShellRe.FindStringSubmatch(text)
	if m == nil {
		return text, false
	}
	out = strings.TrimSpace(m[1])
	if out == "(empty)" {
		out = ""
	}
	if e := strings.TrimSpace(m[2]); e != "" && e != "(none)" {
		out = strings.TrimSpace(out + "\n" + e)
	}
	if m[3] != "0" && m[3] != "(none)" {
		failed = true
		out = strings.TrimSpace(out + fmt.Sprintf(" (exit %s)", m[3]))
	}
	return out, failed
}

func renderQwenParts(parts []any, push pushFunc, toolNames map[string]string, rec map[string]any) {
	status := str(obj(rec, "toolCallResult"), "status")
	var text []any // consecutive text parts are one paragraph of the reply
	flush := func() {
		pushAssistantText(push, partsText(text))
		text = nil
	}
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		switch {
		case obj(pm, "functionCall") != nil:
			flush()
			fc := obj(pm, "functionCall")
			pushToolCall(push, toolNames, str(fc, "id"), str(fc, "name"), fc["args"])
		case obj(pm, "functionResponse") != nil:
			flush()
			fr := obj(pm, "functionResponse")
			name := str(fr, "name")
			if name == "" {
				name = toolNames[str(fr, "id")]
			}
			out, failed := functionResponseText(fr)
			if o, bad := qwenShellText(out); o != out || bad {
				out, failed = o, failed || bad
			}
			pushToolResult(push, name, out, failed || status == "error" || status == "cancelled")
		default:
			text = append(text, pm)
		}
	}
	flush()
}
