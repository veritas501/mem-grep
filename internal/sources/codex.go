package sources

// Codex adapter — SKELETON. Searched like every other agent, but unverified.
//
// Verified against openai/codex source @26ad281 (2026-10-28):
//   - Files: $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<YYYY-MM-DDTHH-MM-SS>-<thread_id>[_<rollout_id>].jsonl
//     and $CODEX_HOME/archived_sessions/. A `_<rollout_id>` suffix appears on reverted threads.
//   - Cold rollouts (older than 7 days) may be compressed in the background to `.jsonl.zst` and
//     decompressed again on resume (codex-rs/rollout/src/compression.rs; feature
//     LocalThreadStoreCompression, under development and off by default). The ingest key is the
//     plain `.jsonl` path so a session keeps its state across that move.
//   - Each line is a RolloutLine: {"timestamp", "ordinal"?, "type", "payload"} (history/src/lib.rs,
//     rollout_payload.rs). The first line is `session_meta`.
//   - Which items carry the conversation, and which user-role text is really the user, follows
//     Codex's own memory pipeline (memories/write/src/rollout_input.rs, rollout/src/policy.rs):
//     only `response_item` (+ inter-agent messages) matter; developer messages, reasoning and
//     compaction are dropped; AGENTS.md / <skill> fragments are removed from user messages; a user
//     message with <environment_context> is harness context; only interactive sources (cli,
//     vscode, custom atlas/chatgpt) are used.
//
// Before enabling it by default:
//   TODO(fixture): generate test/fixtures/codex/ by running Codex in a temporary CODEX_HOME (the
//   TUI, or `codex exec` with the session_meta source patched to "cli", since exec sessions are
//   skipped) and add tests mirroring the omp ones, including a .jsonl.zst copy.
//   TODO(state): Discover reports the compressed file's size for .zst rollouts, while offsets are
//   in decompressed bytes. Ingest treats "size smaller than the stored offset" as a rewritten file
//   and starts over, so a plain→zst move would look like a rewritten file to anything that
//   tracks size and offset. Report the decompressed size, or compare by (key, mtime, record
//   count), before enabling.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// Codex is the Codex CLI adapter.
type Codex struct{}

func (Codex) Kind() ir.SourceKind { return ir.Codex }

var rolloutNameRe = regexp.MustCompile(`^rollout-.*\.jsonl(\.zst)?$`)

// codexKey is the canonical (uncompressed) path of a rollout.
func codexKey(path string) string { return strings.TrimSuffix(path, ".zst") }

func (Codex) Discover(cfg *config.Config) []ir.SourceFile {
	byKey := map[string]ir.SourceFile{}
	for _, root := range cfg.Dirs(ir.Codex) {
		for _, f := range walkFiles(root, rolloutNameRe.MatchString, 4) {
			key := codexKey(f.path)
			if prev, ok := byKey[key]; ok && prev.Path == key {
				continue // prefer the plain file over its compressed sibling, as Codex does
			}
			byKey[key] = ir.SourceFile{Kind: ir.Codex, Path: f.path, Key: key, Size: f.size, MtimeMs: f.mtimeMs}
		}
	}
	// TODO(subagents): subagent threads are separate rollouts whose session_meta has
	// source {"subagent": …} and parent_thread_id. Discover does not read metadata, so ParentKey is
	// only set later by Open (as "codex-thread:<id>"), which still lets ingest treat them as
	// subagents. Ordering parents before children would need the thread id → file map.
	out := make([]ir.SourceFile, 0, len(byKey))
	for _, f := range byKey {
		out = append(out, f)
	}
	return out
}

func (Codex) Sniff(_ string, head []byte) bool {
	var o map[string]any
	if json.Unmarshal(firstLine(head), &o) != nil {
		return false
	}
	_, hasPayload := o["payload"]
	return hasPayload && str(o, "type") == "session_meta"
}

// interactive reports whether a session_meta `source` is one Codex itself uses for memory.
// The field is a lowercase externally tagged enum: "cli", "vscode", "exec", "mcp", "unknown",
// {"custom": "atlas"}, {"subagent": …}, {"internal": …}. Missing means vscode (the serde default).
func interactive(source any) bool {
	switch v := source.(type) {
	case nil:
		return true
	case string:
		return v == "cli" || v == "vscode"
	case map[string]any:
		c, _ := v["custom"].(string)
		return c == "atlas" || c == "chatgpt"
	}
	return false
}

// markedFragment reports whether text (trimmed at the start) is wrapped in start…end markers.
func markedFragment(text, start, end string) bool {
	t := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(t, start) && strings.Contains(t, end)
}

var exitCodeRe = regexp.MustCompile(`Process exited with code (-?\d+)`)

func codexText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	for _, c := range arr(content) {
		cm, _ := c.(map[string]any)
		switch str(cm, "type") {
		case "input_text", "output_text", "text":
			parts = append(parts, str(cm, "text"))
		case "input_image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

func (c Codex) Open(f ir.SourceFile) (*ir.Session, error) {
	buf, err := readMaybeCompressed(f.Path)
	if err != nil {
		return nil, err
	}
	return c.OpenBytes(f, buf, ScanOptions{})
}

// OpenBytes parses a rollout already read (decompressed). ScanOptions are not used yet.
func (Codex) OpenBytes(f ir.SourceFile, buf []byte, _ ScanOptions) (*ir.Session, error) {
	read := parseJSONL(buf)
	if len(read.records) == 0 || str(read.records[0].obj, "type") != "session_meta" {
		return nil, nil
	}
	meta := obj(read.records[0].obj, "payload")
	s := &ir.Session{
		Kind:          ir.Codex,
		File:          f,
		SessionID:     str(meta, "id"),
		Cwd:           str(meta, "cwd"),
		StartedAt:     str(meta, "timestamp"),
		CompleteBytes: read.completeBytes,
		BadLines:      read.badLines,
		// TODO(title): thread names live outside the rollout (session index); not read yet.
	}
	switch ts := str(meta, "thread_source"); {
	case ts == "memory_consolidation" || ts == "guardian_review":
		s.SkipReason = "codex internal thread (" + ts + ")"
	case obj(meta, "source")["subagent"] != nil:
		s.File.ParentKey = "codex-thread:" + str(meta, "parent_thread_id")
		s.File.AgentName = str(meta, "agent_nickname")
	case !interactive(meta["source"]):
		// Codex excludes exec / mcp / internal sessions from its own memory; so do we.
		// TODO: make this configurable if people want their `codex exec` runs remembered.
		s.SkipReason = "non-interactive codex session"
	}
	// Records before this ordinal are the parent's context inherited by a subagent, not its own work.
	startOrdinal := -1.0
	if v, ok := meta["subagent_history_start_ordinal"].(float64); ok {
		startOrdinal = v
	}
	// TODO(forks): `forked_from_id` / `history_base` mean this thread continues another rollout.
	// With history_base the prefix stays in the other file (nothing to do); check whether legacy
	// forks copy the parent's records into the new file, which would look duplicated.

	s.Render = func(from, to int64) []ir.Line {
		var lines []ir.Line
		toolNames := map[string]string{}
		for _, r := range read.records[1:] {
			if ord, ok := r.obj["ordinal"].(float64); ok && startOrdinal >= 0 && ord < startOrdinal {
				continue
			}
			emit := r.start >= from && (to < 0 || r.start < to)
			id := fmt.Sprintf("L%d", r.line)
			if ord, ok := r.obj["ordinal"].(float64); ok {
				id = fmt.Sprintf("o%d", int64(ord))
			}
			ts := str(r.obj, "timestamp")
			push := func(role ir.Role, text string, turnStart bool, success *bool) {
				if emit && !util.IsSpaceOnly(text) {
					lines = append(lines, ir.Line{EntryID: id, TS: ts, Role: role, Text: util.Redact(text), TurnStart: turnStart, Start: r.start, End: r.end, Success: success})
				}
			}
			p := obj(r.obj, "payload")
			switch str(r.obj, "type") {
			case "response_item":
				renderCodexItem(p, push, toolNames)
			case "event_msg":
				switch str(p, "type") {
				case "turn_aborted":
					if str(p, "reason") == "interrupted" {
						push(ir.RoleNote, "  [assistant turn aborted by the user]", false, nil)
					}
				case "thread_rolled_back":
					// TODO(rollback): payload fields unverified (how many turns were dropped).
					push(ir.RoleNote, "[branch: the user rolled back earlier turns; what came after them was abandoned]", true, nil)
				}
			case "inter_agent_communication":
				// TODO(agents): payload shape unverified; render it as a note once known.
			}
			// compacted / turn_context / token_usage_record / world_state / …: bookkeeping or
			// repeats of earlier content.
		}
		return lines
	}
	return s, nil
}

type pushFn func(role ir.Role, text string, turnStart bool, success *bool)

func renderCodexItem(p map[string]any, push pushFn, toolNames map[string]string) {
	switch str(p, "type") {
	case "message":
		switch str(p, "role") {
		case "user":
			if kinds := arr(obj(p, "internal_chat_message_metadata_passthrough")["content_item_kinds"]); len(kinds) > 0 {
				harness := true
				for _, k := range kinds {
					if ks, _ := k.(string); strings.HasPrefix(ks, "user.") {
						harness = false
					}
				}
				if harness {
					return // harness context, not the user
				}
			}
			var said []string
			for _, c := range arr(p["content"]) {
				cm, _ := c.(map[string]any)
				t := str(cm, "text")
				switch {
				case markedFragment(t, "<environment_context>", "</environment_context>"):
					return // the whole message is harness context
				case markedFragment(t, "# AGENTS.md instructions", "</INSTRUCTIONS>"), markedFragment(t, "<skill>", "</skill>"):
					continue
				}
				if s := codexText([]any{c}); s != "" {
					said = append(said, s)
				}
			}
			text := strings.TrimSpace(strings.Join(said, "\n"))
			switch {
			case text == "":
			case strings.HasPrefix(text, "<subagent_notification>") || strings.HasPrefix(text, "Message Type:"):
				push(ir.RoleNote, "[message from another agent: "+util.TruncateMiddle(text, 1200)+"]", false, nil)
			default:
				push(ir.RoleUser, "USER: "+util.TruncateMiddle(text, 4000), true, nil)
			}
		case "assistant":
			// phase "commentary" (progress notes) and "final_answer" both render as agent text.
			if t := strings.TrimSpace(codexText(p["content"])); t != "" {
				push(ir.RoleAssistant, "ASSISTANT: "+util.TruncateMiddle(t, 2500), false, nil)
			}
		}
		// developer / system: instructions, not the story
	case "function_call":
		name := str(p, "name")
		toolNames[str(p, "call_id")] = name
		var args any = str(p, "arguments") // a JSON string on the wire
		var parsed map[string]any
		if json.Unmarshal([]byte(str(p, "arguments")), &parsed) == nil {
			args = parsed
		}
		push(ir.RoleTool, fmt.Sprintf("  → %s(%s)", name, SummarizeArgs(args)), false, nil)
	case "custom_tool_call":
		name := str(p, "name")
		toolNames[str(p, "call_id")] = name
		push(ir.RoleTool, fmt.Sprintf("  → %s(%s)", name, util.Truncate(spaceRe.ReplaceAllString(str(p, "input"), " "), 400)), false, nil)
	case "local_shell_call":
		var cmd []string
		for _, c := range arr(obj(p, "action")["command"]) {
			cmd = append(cmd, fmt.Sprint(c))
		}
		toolNames[str(p, "call_id")] = "shell"
		push(ir.RoleTool, fmt.Sprintf("  → shell(command=%s)", util.Truncate(strings.Join(cmd, " "), 400)), false, nil)
	case "web_search_call":
		push(ir.RoleTool, fmt.Sprintf("  → web_search(%s)", SummarizeArgs(p["action"])), false, nil)
	case "function_call_output", "custom_tool_call_output":
		name := toolNames[str(p, "call_id")]
		if name == "" {
			name = "tool"
		}
		out := strings.TrimSpace(codexText(p["output"]))
		if name == "request_user_input" {
			// The user's answer to a question the agent asked: Codex counts it as the human speaking.
			push(ir.RoleUser, "USER (answering the agent): "+util.TruncateMiddle(out, 2000), false, nil)
			return
		}
		// Outputs are plain text; command runs end with "Process exited with code N".
		// TODO(success): other tools have no structured status; consider error-text heuristics.
		var ok *bool
		if m := exitCodeRe.FindStringSubmatch(out); m != nil {
			code, _ := strconv.Atoi(m[1])
			ok = ir.Bool(code == 0)
		}
		if ok != nil && !*ok {
			push(ir.RoleResult, fmt.Sprintf("  ✗ %s failed: %s", name, util.TruncateMiddle(out, 700)), false, ok)
		} else {
			push(ir.RoleResult, fmt.Sprintf("  ✓ %s: %s", name, util.Tail(out, 300)), false, ok)
		}
	}
	// reasoning, compaction, tool_search_*, image_generation_call, …: dropped
}
