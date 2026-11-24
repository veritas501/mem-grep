package testutil

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// ZCodeWorkspace is the workspace identity of the main session in ZCodeHome; the memory
// directory of its project is named after its hash.
const ZCodeWorkspace = "local:/home/dev/shop"

// ZCodeHome writes a ZCode storage root (…/.zcode/cli) and returns its path: db/db.sqlite shaped
// after apps/zcode-cli/packages/adapters/src/storage/session-store/schema.baseline.sql at ZCode
// v4.0.0 — only the columns the adapter reads — with rows shaped after
// packages/contracts/src/interfaces/session-store.port.ts, and memory files laid out as
// packages/core/src/memory writes them. It is not written by ZCode: it holds the cases a scripted
// run does not produce (test/fixtures/zcode is the fixture ZCode wrote itself).
func ZCodeHome(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".zcode", "cli")
	if err := os.MkdirAll(filepath.Join(root, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "db", "db.sqlite"))
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
	exec(`CREATE TABLE session (id text primary key, project_id text, workspace_id text, parent_id text, slug text, directory text, title text,
		revert text, time_created integer, time_updated integer, time_deleted integer, task_type text not null default 'interactive')`)
	exec(`CREATE TABLE message (id text primary key, session_id text, time_created integer, time_updated integer, data text, sequence integer)`)
	exec(`CREATE TABLE part (id text primary key, message_id text, session_id text, time_created integer, time_updated integer, data text, sequence integer)`)

	const (
		main = "sess_11111111-aaaa-4bbb-8ccc-000000000001"
		sub  = "sess_subagent_agent_22222222-aaaa-4bbb-8ccc-000000000002"
		fork = "sess_33333333-aaaa-4bbb-8ccc-000000000003"
		gone = "sess_44444444-aaaa-4bbb-8ccc-000000000004"
	)
	// A rewind keeps the first five messages; msg_7 is the last one stored before it.
	revert := `{"messageID":"msg_5","kind":"conversation_rewind","scope":"conversation","targetMessageID":"msg_6",
		"keptMessageIDs":["msg_1","msg_2","msg_3","msg_4","msg_5"],"branchCutAfterMessageID":"msg_7","branchGeneration":1}`
	exec(`INSERT INTO session VALUES (?, 'p1', ?, NULL, 's', '/home/dev/shop', 'Fix order tests', ?, 1793000000000, 1793000020000, NULL, 'interactive')`, main, ZCodeWorkspace, revert)
	exec(`INSERT INTO session VALUES (?, 'p1', NULL, ?, 's2', '/home/dev/shop', 'find the test db config', NULL, 1793000004000, 1793000005000, NULL, 'subagent_child')`, sub, main)
	exec(`INSERT INTO session VALUES (?, 'p1', ?, ?, 's3', '/home/dev/shop', 'Forked', NULL, 1793000030000, 1793000031000, NULL, 'fork')`, fork, ZCodeWorkspace, main)
	exec(`INSERT INTO session VALUES (?, 'p1', NULL, NULL, 's4', '/home/dev/old', 'Deleted', NULL, 1793000000000, 1793000001000, 1793000002000, 'interactive')`, gone)

	seq := map[string]int{}
	msg := func(id, sid string, created int64, data string) {
		exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`, id, sid, created, created, data, seq[sid])
		seq[sid]++
	}
	part := func(id, mid, sid string, data string) {
		exec(`INSERT INTO part VALUES (?, ?, ?, 0, 0, ?, ?)`, id, mid, sid, data, seq[mid])
		seq[mid]++
	}
	const (
		user      = `"semantics":{"origin":"real_user","kind":"user_prompt","uiVisibility":"visible","providerVisibility":"visible","transcriptVisibility":"visible"}`
		assistant = `"semantics":{"origin":"agent_runtime","kind":"assistant_response","uiVisibility":"visible","providerVisibility":"visible","transcriptVisibility":"visible"}`
	)
	// a reminder the runtime injects for the model only
	hidden := func(source string) string {
		return `"synthetic":true,"visibility":"model-only","semantics":{"origin":"agent_runtime","kind":"system_reminder","source":"` + source + `","uiVisibility":"hidden","providerVisibility":"visible","transcriptVisibility":"hidden"}`
	}

	// turn 1: the user's request, injected context, a failing command, an interrupted reply
	msg("msg_1", main, 1793000001000, `{"role":"user","time":{"created":1793000001000},"agent":"zcode-agent","model":{},`+user+`}`)
	part("prt_1a", "msg_1", main, `{"type":"text","text":"用 pnpm 跑测试"}`)
	part("prt_1b", "msg_1", main, `{"type":"file","mime":"text/plain","filename":"package.json","url":"file:///x"}`)
	msg("msg_2", main, 1793000001500, `{"role":"user","time":{"created":1793000001500},"agent":"zcode-agent","model":{},`+hidden("environment")+`}`)
	part("prt_2a", "msg_2", main, `{"type":"text","text":"# Environment\nYou have been invoked in /home/dev/shop","synthetic":true,"metadata":{"source":"environment","visibility":"model-only"}}`)
	msg("msg_3", main, 1793000002000, `{"role":"assistant","time":{"created":1793000002000,"completed":1793000003000},"parentID":"msg_1","agent":"zcode-agent","mode":"yolo","finish":"tool-calls","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"},`+assistant+`}`)
	part("prt_3a", "msg_3", main, `{"type":"step-start"}`)
	part("prt_3b", "msg_3", main, `{"type":"reasoning","text":"hidden reasoning","blockIndex":0,"time":{"start":1}}`)
	// stored before the text it follows: blockIndex is the order the model produced them in
	part("prt_3c", "msg_3", main, `{"type":"tool","callID":"call_1","tool":"Bash","blockIndex":2,"state":{"status":"completed","input":{"command":"pnpm test","description":"run tests"},"output":"Exit code 1\nconnect ECONNREFUSED 127.0.0.1:5432","title":"Bash","metadata":{"schemaVersion":1,"providerIsError":true},"time":{"start":1,"end":2}}}`)
	part("prt_3d", "msg_3", main, `{"type":"text","text":"先跑一遍测试","blockIndex":1}`)
	part("prt_3e", "msg_3", main, `{"type":"tool","callID":"call_2","tool":"Read","blockIndex":3,"state":{"status":"completed","input":{"file_path":"/home/dev/shop/.env.test"},"output":"DB_PORT=5433","title":"Read","metadata":{"schemaVersion":1,"readFileState":{"tool":"Read","path":"/home/dev/shop/.env.test","content":"SNAPSHOT_ONLY_IN_METADATA"}},"time":{"start":1,"end":2}}}`)
	part("prt_3f", "msg_3", main, `{"type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}`)
	msg("msg_4", main, 1793000004000, `{"role":"assistant","time":{"created":1793000004000},"parentID":"msg_1","agent":"zcode-agent","mode":"yolo","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"},"error":{"name":"AiSdkModelAdapterError","data":{"message":"Model request was cancelled.","code":"model_request_cancelled","attribution":{"source":"runtime","reason":"cancelled"}}},`+assistant+`}`)
	part("prt_4a", "msg_4", main, `{"type":"tool","callID":"call_3","tool":"Bash","blockIndex":0,"state":{"status":"running","input":{"command":"sleep 100"},"time":{"start":1}}}`)
	msg("msg_5", main, 1793000004500, `{"role":"user","time":{"created":1793000004500},"agent":"zcode-agent","model":{},`+hidden("interrupt_marker")+`}`)
	part("prt_5a", "msg_5", main, `{"type":"text","text":"[Request interrupted by user]","synthetic":true,"metadata":{"source":"interrupt_marker","visibility":"model-only"}}`)

	// turn 2: sent, answered, then taken back by a rewind
	msg("msg_6", main, 1793000005000, `{"role":"user","time":{"created":1793000005000},"agent":"zcode-agent","model":{},`+user+`}`)
	part("prt_6a", "msg_6", main, `{"type":"text","text":"把数据库换成 mysql"}`)
	msg("msg_7", main, 1793000006000, `{"role":"assistant","time":{"created":1793000006000,"completed":1793000006500},"parentID":"msg_6","agent":"zcode-agent","mode":"yolo","finish":"stop","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"},`+assistant+`}`)
	part("prt_7a", "msg_7", main, `{"type":"text","text":"好的，改成 mysql。","blockIndex":0}`)

	// turn 3: the message the user sent instead, with runtime context around it
	msg("msg_8", main, 1793000010000, `{"role":"user","time":{"created":1793000010000},"agent":"zcode-agent","model":{},`+user+`}`)
	part("prt_8a", "msg_8", main, `{"type":"text","text":"测试库的端口是 5433"}`)
	msg("msg_9", main, 1793000010500, `{"role":"user","time":{"created":1793000010500},"agent":"zcode-agent","model":{},"source":"todo_reminder","synthetic":true,"visibility":"model-only","semantics":{"origin":"agent_runtime","kind":"todo_reminder","source":"todo_reminder","uiVisibility":"hidden","providerVisibility":"visible","transcriptVisibility":"hidden"}}`)
	part("prt_9a", "msg_9", main, `{"type":"text","text":"The TodoWrite tool hasn't been used recently.","synthetic":true}`)
	msg("msg_10", main, 1793000011000, `{"role":"assistant","time":{"created":1793000011000,"completed":1793000012000},"parentID":"msg_8","agent":"zcode-agent","mode":"yolo","finish":"stop","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"},`+assistant+`}`)
	part("prt_10a", "msg_10", main, `{"type":"tool","callID":"call_4","tool":"Edit","state":{"status":"error","input":{"file_path":"/home/dev/shop/.env.test","old_string":"5432","new_string":"5433"},"error":"String to replace not found in file.","metadata":{},"time":{"start":1,"end":2}}}`)
	part("prt_10b", "msg_10", main, `{"type":"text","text":"端口已经是 5433，测试通过了。"}`)
	msg("msg_11", main, 1793000013000, `{"role":"assistant","time":{"created":1793000013000},"parentID":"msg_8","agent":"zcode-agent","mode":"yolo","finish":"completed","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"},"semantics":{"origin":"system","kind":"timeline_event","uiVisibility":"visible","providerVisibility":"hidden","transcriptVisibility":"visible"}}`)
	part("prt_11a", "msg_11", main, `{"type":"timeline","timelineType":"model_change","display":"separator","status":"completed","toModel":{"label":"timeline label"}}`)
	msg("msg_12", main, 1793000014000, `{"role":"user","time":{"created":1793000014000},"agent":"zcode-agent","model":{},"summary":{"diffs":[]},"semantics":{"origin":"agent_runtime","kind":"compact_summary","uiVisibility":"hidden","providerVisibility":"visible","transcriptVisibility":"hidden"}}`)
	part("prt_12a", "msg_12", main, `{"type":"text","text":"compaction summary that repeats everything","synthetic":true}`)
	msg("msg_13", main, 1793000015000, `{"role":"assistant","time":{"created":1793000015000},"parentID":"msg_8","agent":"zcode-agent","mode":"yolo","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"},"error":{"name":"AiSdkModelAdapterError","data":{"message":"[1308] usage limit reached","code":"model_rate_limited","attribution":{"source":"provider","reason":"rate_limited"}}},`+assistant+`}`)

	// the subagent: its "user" is the agent that delegated the task
	msg("msg_s1", sub, 1793000004100, `{"role":"user","time":{"created":1793000004100},"agent":"zcode-Explore","model":{},`+user+`}`)
	part("prt_s1a", "msg_s1", sub, `{"type":"text","text":"find the test db config"}`)
	msg("msg_s2", sub, 1793000004200, `{"role":"assistant","time":{"created":1793000004200,"completed":1793000004300},"parentID":"msg_s1","agent":"zcode-Explore","mode":"yolo","finish":"stop","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"},`+assistant+`}`)
	part("prt_s2a", "msg_s2", sub, `{"type":"text","text":"It is in .env.test"}`)

	// the fork: a conversation of its own, although it has a parent
	msg("msg_f1", fork, 1793000030000, `{"role":"user","time":{"created":1793000030000},"agent":"zcode-agent","model":{},`+user+`}`)
	part("prt_f1a", "msg_f1", fork, `{"type":"text","text":"再试试 sqlite"}`)
	msg("msg_g1", gone, 1793000000500, `{"role":"user","time":{"created":1793000000500},"agent":"zcode-agent","model":{},`+user+`}`)
	part("prt_g1a", "msg_g1", gone, `{"type":"text","text":"a deleted conversation"}`)

	// memories: the project of the main session (found through its workspace identity), and one
	// whose project only the memory's origin session names
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, "memories", "projects", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project := ZCodeMemoryProject(ZCodeWorkspace)
	write(project+"/memory/MEMORY.md", "- [Test database](feedback_test_db.md) — tests run against the real database on port 5433\n")
	write(project+"/memory/feedback_test_db.md", "---\nname: test-database\ndescription: \"tests run against the real database on port 5433\"\nmetadata:\n  node_type: memory\n  type: feedback\n  originSessionId: "+main+"\n---\n\nNever mock the database in integration tests.\n\n**Why:** a mocked migration passed while production failed.\n**How to apply:** start postgres on 5433 first; the token is sk-abcdefghijklmnopqrstuvwxyz012345.\n")
	write("project-0000000000000000/memory/notes/user_role.md", "---\nname: user-role\ndescription: >-\n  the user maintains\n  the shop backend\nmetadata:\n  type: user\n  originSessionId: "+fork+"\n---\nPrefers pnpm over npm.\n")
	return root
}

// ZCodeMemoryProject is the directory ZCode keeps a workspace's memories in, for a workspace
// identity (core/src/memory/project-root.ts resolveProjectMemoryRoot).
func ZCodeMemoryProject(workspaceIdentity string) string {
	sum := sha256.Sum256([]byte(workspaceIdentity))
	return "project-" + hex.EncodeToString(sum[:])[:16]
}
