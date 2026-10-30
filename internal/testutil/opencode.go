package testutil

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// OpenCodeDB writes a database shaped after packages/core/src/session/sql.ts at opencode v1.19.5 — only the columns
// the adapter reads — filled with rows shaped after packages/schema/src/v1/session.ts. It is not a
// fixture written by opencode (see TODO(fixture) in opencode.go).
func OpenCodeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
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
	exec(`CREATE TABLE session (id text primary key, project_id text, parent_id text, slug text, directory text, title text, time_created integer, time_updated integer)`)
	exec(`CREATE TABLE message (id text primary key, session_id text, time_created integer, time_updated integer, data text)`)
	exec(`CREATE TABLE part (id text primary key, message_id text, session_id text, time_created integer, time_updated integer, data text)`)
	exec(`INSERT INTO session VALUES ('ses_main', 'p1', NULL, 's', '/home/dev/shop', 'Fix order tests', 1793000000000, 1793000009000)`)
	exec(`INSERT INTO session VALUES ('ses_sub', 'p1', 'ses_main', 's2', '/home/dev/shop', 'Explore (@explore subagent)', 1793000004000, 1793000005000)`)
	msg := func(id, sid string, created int64, data string) {
		exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?)`, id, sid, created, created, data)
	}
	part := func(id, mid, sid string, data string) {
		exec(`INSERT INTO part VALUES (?, ?, ?, 0, 0, ?)`, id, mid, sid, data)
	}
	msg("msg_1", "ses_main", 1793000001000, `{"role":"user","time":{"created":1793000001000},"agent":"build","model":{"providerID":"x","modelID":"y"}}`)
	part("prt_1a", "msg_1", "ses_main", `{"type":"text","text":"用 pnpm 跑测试"}`)
	part("prt_1b", "msg_1", "ses_main", `{"type":"text","text":"Called the Read tool with the following input: {}","synthetic":true}`)
	part("prt_1c", "msg_1", "ses_main", `{"type":"file","mime":"text/plain","filename":"package.json","url":"file:///x"}`)
	msg("msg_2", "ses_main", 1793000002000, `{"role":"assistant","time":{"created":1793000002000},"parentID":"msg_1","path":{"cwd":"/home/dev/shop","root":"/home/dev/shop"}}`)
	part("prt_2a", "msg_2", "ses_main", `{"type":"step-start"}`)
	part("prt_2b", "msg_2", "ses_main", `{"type":"reasoning","text":"hidden reasoning","time":{"start":1}}`)
	part("prt_2c", "msg_2", "ses_main", `{"type":"text","text":"先跑一遍测试"}`)
	part("prt_2d", "msg_2", "ses_main", `{"type":"tool","callID":"call_1","tool":"bash","state":{"status":"error","input":{"command":"pnpm test","description":"run tests"},"error":"connect ECONNREFUSED 127.0.0.1:5432","time":{"start":1,"end":2}}}`)
	part("prt_2e", "msg_2", "ses_main", `{"type":"tool","callID":"call_2","tool":"read","state":{"status":"completed","input":{"filePath":"/home/dev/shop/.env.test"},"output":"DB_PORT=5433","title":"read","metadata":{},"time":{"start":1,"end":2}}}`)
	part("prt_2f", "msg_2", "ses_main", `{"type":"tool","callID":"call_3","tool":"bash","state":{"status":"running","input":{"command":"sleep 100"},"time":{"start":1}}}`)
	msg("msg_3", "ses_main", 1793000003000, `{"role":"assistant","time":{"created":1793000003000},"parentID":"msg_1","summary":true,"path":{"cwd":"/","root":"/"}}`)
	part("prt_3a", "msg_3", "ses_main", `{"type":"text","text":"compaction summary that repeats everything"}`)
	msg("msg_4", "ses_main", 1793000004000, `{"role":"assistant","time":{"created":1793000004000},"error":{"name":"MessageAbortedError","data":{"message":"aborted"}},"path":{"cwd":"/","root":"/"}}`)
	msg("msg_5", "ses_sub", 1793000004500, `{"role":"user","time":{"created":1793000004500}}`)
	part("prt_5a", "msg_5", "ses_sub", `{"type":"text","text":"find the test db config"}`)
	return path
}
