package sources

// Shared by the Gemini CLI and Qwen Code adapters: both store Google GenAI `Content`
// ({role, parts[]}) where a part is {text}, {text, thought: true}, {functionCall: {id, name, args}}
// or {functionResponse: {id, name, response: {output} | {error}}}.

import (
	"fmt"
	"strings"

	"github.com/veritas501/mem-grep/internal/ir"
	"github.com/veritas501/mem-grep/internal/util"
)

// contentParts normalises a PartListUnion: a string, one part, or a list of parts.
func contentParts(v any) []any {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []any{map[string]any{"text": x}}
	case map[string]any:
		return []any{x}
	case []any:
		var out []any
		for _, e := range x {
			out = append(out, contentParts(e)...)
		}
		return out
	}
	return nil
}

// partsText joins the visible text of parts (thought summaries are not visible).
func partsText(parts []any) string {
	var texts []string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if truthy(pm["thought"]) {
			continue
		}
		if t, ok := pm["text"].(string); ok && t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

// functionResponseText returns what a tool answered and whether it was an error.
func functionResponseText(fr map[string]any) (string, bool) {
	resp := obj(fr, "response")
	if e, ok := resp["error"]; ok && truthy(e) {
		if s, ok := e.(string); ok {
			return s, true
		}
		return jsonText(e), true
	}
	if o, ok := resp["output"]; ok {
		if s, ok := o.(string); ok {
			return s, false
		}
		return jsonText(o), false
	}
	if len(resp) == 0 {
		return "", false
	}
	return jsonText(resp), false
}

type pushFunc = func(role ir.Role, text string, turnStart bool, success *bool)

// pushToolCall renders a tool call line and remembers the tool's name by call id.
func pushToolCall(push pushFunc, toolNames map[string]string, id, name string, args any) {
	if id != "" {
		toolNames[id] = name
	}
	push(ir.RoleTool, fmt.Sprintf("  → %s(%s)", name, SummarizeArgs(args)), false, nil)
}

// pushToolResult renders a tool result like the other adapters: failures keep their message,
// shell-like tools their output tail, the rest a one-line preview.
func pushToolResult(push pushFunc, name, text string, failed bool) {
	if name == "" {
		name = "tool"
	}
	text = strings.TrimSpace(text)
	switch {
	case failed:
		push(ir.RoleResult, fmt.Sprintf("  ✗ %s failed: %s", name, util.TruncateMiddle(text, 700)), false, ir.Bool(false))
	case shellToolRe.MatchString(name):
		push(ir.RoleResult, fmt.Sprintf("  ✓ %s: %s", name, util.Tail(text, 300)), false, ir.Bool(true))
	default:
		suffix := ""
		if text != "" {
			suffix = ": " + util.Truncate(spaceRe.ReplaceAllString(text, " "), 120)
		}
		push(ir.RoleResult, fmt.Sprintf("  ✓ %s%s", name, suffix), false, ir.Bool(true))
	}
}

// pushUser renders a user message as the opening of a turn.
func pushUser(push pushFunc, text string) {
	if text = strings.TrimSpace(text); text != "" {
		push(ir.RoleUser, "USER: "+util.TruncateMiddle(text, 4000), true, nil)
	}
}

// pushAssistantText renders text the model wrote.
func pushAssistantText(push pushFunc, text string) {
	if t := strings.TrimSpace(text); t != "" {
		push(ir.RoleAssistant, "ASSISTANT: "+util.TruncateMiddle(t, 2500), false, nil)
	}
}
