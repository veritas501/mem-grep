package testutil

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Conversation ids of AntigravityHome.
const (
	AntigravityMain     = "1f8f3aa6-8b94-4db7-923e-c03525ef43d7"
	AntigravitySubagent = "6ec5d501-66b7-4e21-b683-db1ebda6b10f"
	AntigravityEmpty    = "93f67f1d-6fb1-4ad4-9abd-1f91daefd923"
)

// antigravityMain is a main conversation: tools that succeed and fail, a shell command that
// exits non-zero, a reply blocked by the model, a rewind (step 6 sent again), runtime reminders,
// a background command's result, a message from the subagent and a compaction summary.
var antigravityMain = []map[string]any{
	{"step_index": 0, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE", "created_at": "2026-11-24T11:01:27Z",
		"content": "<USER_REQUEST>\n用 pnpm 跑测试\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-24T03:01:27-08:00.\n</ADDITIONAL_METADATA>\n<USER_SETTINGS_CHANGE>\nThe user changed setting `Model Selection` to Gemini.\n</USER_SETTINGS_CHANGE>"},
	{"step_index": 1, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-24T11:01:28Z",
		"thinking": "secret thoughts", "content": "先看目录。",
		"tool_calls": []any{
			map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "pnpm test", "WaitMsBeforeAsync": 5000, "toolSummary": "run the tests"}},
			map[string]any{"name": "read_file", "args": map[string]any{"Path": "/home/dev/shop/notes.txt", "StartLine": 1}},
		}},
	{"step_index": 2, "source": "MODEL", "type": "GENERIC", "status": "DONE", "created_at": "2026-11-24T11:01:30Z",
		"content": "Created At: 2026-11-24T03:01:30-08:00\nCompleted At: 2026-11-24T03:01:31-08:00\nCreated At: 2026-11-24T03:01:30-08:00\nCompleted At: 2026-11-24T03:01:31-08:00\nCommand exited with code 1.\n\nCommand output:\nError: connect ECONNREFUSED 127.0.0.1:5432"},
	{"step_index": 3, "source": "MODEL", "type": "GENERIC", "status": "DONE", "created_at": "2026-11-24T11:01:30Z",
		"content": "Created At: 2026-11-24T03:01:30-08:00\nCompleted At: 2026-11-24T03:01:31-08:00\nShowing lines 1-1 of 1\n1: start test-db before pnpm test"},
	{"step_index": 4, "source": "SYSTEM", "type": "SYSTEM_MESSAGE", "status": "DONE", "created_at": "2026-11-24T11:01:32Z",
		"content": "The following is a <SYSTEM_MESSAGE> not actually sent by the user.\n\n<SYSTEM_MESSAGE>\nReminder: work efficiently.\n</SYSTEM_MESSAGE>"},
	{"step_index": 5, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-24T11:01:33Z",
		"tool_calls": []any{map[string]any{"name": "edit_file", "args": map[string]any{"Path": "/home/dev/shop/a.ts", "Target": "x", "Replacement": "y"}}}},
	{"step_index": 6, "source": "MODEL", "type": "GENERIC", "status": "ERROR", "created_at": "2026-11-24T11:01:34Z",
		"error":   "target content not found in the file",
		"content": "Created At: 2026-11-24T03:01:34-08:00\nCompleted At: 2026-11-24T03:01:34-08:00\nEncountered error in step execution: target content not found in the file"},
	{"step_index": 7, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "ERROR", "created_at": "2026-11-24T11:01:35Z",
		"error": "response was blocked by content safety filters"},
	{"step_index": 8, "source": "SYSTEM", "type": "ERROR_MESSAGE", "status": "DONE", "created_at": "2026-11-24T11:01:36Z",
		"error": "A response attempt was blocked\nRetries remaining: 3", "content": "Error: A response attempt was blocked\nRetries remaining: 3"},
	// the user goes back and sends again from step 7
	{"step_index": 7, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE", "created_at": "2026-11-24T11:02:00Z",
		"content": "<USER_REQUEST>\n换个思路，先启动 test-db\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-24T03:02:00-08:00.\n</ADDITIONAL_METADATA>"},
	{"step_index": 8, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-24T11:02:01Z",
		"tool_calls": []any{
			map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "docker start test-db", "WaitMsBeforeAsync": 500}},
			map[string]any{"name": "invoke_subagent", "args": map[string]any{"TypeName": "research", "Role": "auditor", "Prompt": "audit the mysql config"}},
		}},
	{"step_index": 9, "source": "MODEL", "type": "GENERIC", "status": "RUNNING", "created_at": "2026-11-24T11:02:02Z",
		"content": "Created At: 2026-11-24T03:02:02-08:00\nTool is running as a background task with task id: " + AntigravityMain + "/task-9"},
	{"step_index": 10, "source": "MODEL", "type": "GENERIC", "status": "DONE", "created_at": "2026-11-24T11:02:02Z",
		"content": "Created At: 2026-11-24T03:02:02-08:00\nCompleted At: 2026-11-24T03:02:03-08:00\nCreated the following subagent: " + AntigravitySubagent},
	{"step_index": 11, "source": "SYSTEM", "type": "SYSTEM_MESSAGE", "status": "DONE", "created_at": "2026-11-24T11:02:10Z",
		"content": "The following is a <SYSTEM_MESSAGE> not actually sent by the user.\n\n<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-24T11:02:10Z sender=" + AntigravityMain + "/task-9 priority=MESSAGE_PRIORITY_HIGH content=Task id \"task-9\" finished with result:\nCommand exited with code 0.\n\nCommand output:\ntest-db started on port 5433\n</SYSTEM_MESSAGE>"},
	{"step_index": 12, "source": "SYSTEM", "type": "SYSTEM_MESSAGE", "status": "DONE", "created_at": "2026-11-24T11:02:20Z",
		"content": "The following is a <SYSTEM_MESSAGE> not actually sent by the user.\n\n<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-24T11:02:20Z sender=" + AntigravitySubagent + " priority=MESSAGE_PRIORITY_HIGH content=the mysql config uses sqlite in tests\n</SYSTEM_MESSAGE>"},
	{"step_index": 13, "source": "SYSTEM", "type": "CHECKPOINT", "status": "DONE", "created_at": "2026-11-24T11:02:30Z",
		"content": "# Resuming from a compaction\n\nsummary with usage limit details"},
	{"step_index": 14, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-24T11:02:31Z",
		"content": "test-db 已启动，测试通过。"},
}

// antigravitySubagent is the subagent the main conversation started: its task arrives as a
// message from the parent.
var antigravitySubagent = []map[string]any{
	{"step_index": 0, "source": "SYSTEM", "type": "SYSTEM_MESSAGE", "status": "DONE", "created_at": "2026-11-24T11:02:03Z",
		"content": "The following is a <SYSTEM_MESSAGE> not actually sent by the user.\n\n<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-24T11:02:03Z sender=" + AntigravityMain + " priority=MESSAGE_PRIORITY_HIGH content=audit the mysql config\n</SYSTEM_MESSAGE>"},
	{"step_index": 1, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-24T11:02:05Z",
		"tool_calls": []any{map[string]any{"name": "send_message", "args": map[string]any{"Recipient": AntigravityMain, "Message": "the mysql config uses sqlite in tests"}}}},
	{"step_index": 2, "source": "MODEL", "type": "GENERIC", "status": "DONE", "created_at": "2026-11-24T11:02:06Z",
		"content": "Created At: 2026-11-24T03:02:06-08:00\nCompleted At: 2026-11-24T03:02:06-08:00\nMessage sent to \"" + AntigravityMain + "\""},
}

// AntigravityHome writes an Antigravity storage root (~/.gemini/antigravity-cli) and returns its
// path: transcripts and conversation_summaries.db shaped after what agy 1.5 writes (see
// internal/sources/antigravity.go), with made-up content. It is not written by agy: it holds the
// cases a scripted run does not produce (rewinds, blocked replies, background tasks, messages
// between agents). AntigravityEmpty has a summary but no transcript, like a conversation with no
// steps; the transcript.jsonl and chunks beside the main transcript must not be read.
func AntigravityHome(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli")
	write := func(id string, steps []map[string]any) {
		t.Helper()
		logs := filepath.Join(root, "brain", id, ".system_generated", "logs")
		if err := os.MkdirAll(filepath.Join(logs, "chunks", "transcript_full"), 0o755); err != nil {
			t.Fatal(err)
		}
		var buf []byte
		for _, s := range steps {
			line, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			buf = append(append(buf, line...), '\n')
		}
		for _, name := range []string{"transcript_full.jsonl", "transcript.jsonl", "chunks/transcript_full/00000000.jsonl"} {
			if err := os.WriteFile(filepath.Join(logs, name), buf, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(AntigravityMain, antigravityMain)
	write(AntigravitySubagent, antigravitySubagent)
	if err := os.MkdirAll(filepath.Join(root, "brain", AntigravityEmpty), 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// only the columns the adapter reads
	exec("CREATE TABLE `conversation_summaries` (`conversation_id` text, `title` text NOT NULL DEFAULT \"\", `workspace_uris` text NOT NULL, `parent_conversation_id` text NOT NULL DEFAULT \"\", `agent_name` text NOT NULL DEFAULT \"\", PRIMARY KEY (`conversation_id`))")
	exec("INSERT INTO conversation_summaries VALUES (?, 'Run The Tests', '[\"file:///home/dev/shop\"]', '', '')", AntigravityMain)
	exec("INSERT INTO conversation_summaries VALUES (?, '', '[\"file:///home/dev/shop\"]', ?, 'research')", AntigravitySubagent, AntigravityMain)
	exec("INSERT INTO conversation_summaries VALUES (?, 'hi', '', '', '')", AntigravityEmpty)
	return root
}
