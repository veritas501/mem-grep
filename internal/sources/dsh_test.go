package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/veritas501/mem-grep/internal/config"
	"github.com/veritas501/mem-grep/internal/ir"
)

// Rule tests on small records shaped after the v4 session log in deepseek-harness
// (packages/core/session/src/types.ts, packages/llm/llm/src/message.ts). Not a format fixture:
// see TODO(fixture) in dsh.go.

const dshSession = `{"type":"session","version":4,"id":"session-11111111-2222-3333-4444-555555555555","createdAt":1762000000000,"cwd":"/home/dev/shop","isSeeded":false,"delegationDepth":0}
{"type":"system/message","seq":0,"time":1762000001000,"data":{"turn":1,"step":1,"message":{"role":"system","content":[{"type":"text","text":"You are an AI agent powered by DeepSeek Harness."}],"source":{"kind":"system-prompt"},"id":"m0"}}}
{"type":"user/message","seq":1,"time":1762000002000,"data":{"content":[{"type":"text","text":"当前时间是 2026-11-01。"}],"source":{"kind":"time-context"},"role":"user","id":"m1"}}
{"type":"command/run","seq":2,"time":1762000003000,"data":{"commandId":"cmd-1","name":"model","args":" deepseek-flash","source":{"kind":"user"}}}
{"type":"user/message","seq":3,"time":1762000004000,"data":{"content":[{"type":"text","text":"用 pnpm 跑测试"}],"source":{"kind":"user"},"role":"user","id":"m3"}}
{"type":"assistant/message","seq":4,"time":1762000005000,"data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"reasoning","text":"hidden reasoning"},{"type":"text","text":"先跑一遍"},{"type":"tool-call","id":"call_1","name":"bash","arguments":"{\"command\":\"pnpm test\"}"}],"source":{"kind":"model","provider":"p","model":"m"},"id":"m4"},"stream":[]}}
{"type":"tool/call","seq":5,"time":1762000006000,"data":{"turn":1,"step":1,"callId":"call_1","name":"bash","arguments":"{\"command\":\"pnpm test\"}"}}
{"type":"tool/result","seq":6,"time":1762000007000,"data":{"turn":1,"step":1,"message":{"role":"tool","source":{"kind":"tool","callId":"call_1"},"toolCallId":"call_1","content":[{"type":"text","text":"connect ECONNREFUSED 127.0.0.1:5432"}],"isError":true,"id":"m6"}}}
{"type":"turn/end","seq":7,"time":1762000008000,"data":{"turn":1,"reason":{"kind":"completed"}}}
{"type":"user/message","seq":8,"time":1762000009000,"data":{"content":[{"type":"text","text":"The user changed the approval policy."}],"source":{"kind":"user-approval"},"role":"user","id":"m8"}}
{"type":"user/message","seq":9,"time":1762000010000,"data":{"content":[{"type":"text","text":"顺便看下 .env.test"}],"source":{"kind":"user"},"role":"user","id":"m9"}}
{"type":"assistant/message","seq":10,"time":1762000011000,"data":{"turn":2,"step":1,"message":{"role":"assistant","content":[{"type":"text","text":"需要先启动数据库。"}],"source":{"kind":"model","provider":"p","model":"m"},"id":"m10"},"stream":[],"interrupted":true}}
{"type":"turn/end","seq":11,"time":1762000012000,"data":{"turn":2,"reason":{"kind":"aborted","reason":{"kind":"user"}}}}
{"type":"session/title","seq":12,"time":1762000013000,"data":{"title":"Fix order tests","source":{"kind":"fallback"}}}
{"type":"turn/end","seq":13,"time":1762000014000,"data":{"turn":3,"reason":{"kind":"error","error":{"message":"boom","code":"UNKNOWN"}}}}
`

func TestDshRendering(t *testing.T) {
	s, err := Dsh{}.OpenBytes(ir.SourceFile{Kind: ir.Dsh, Path: "/x/session.v4.jsonl"}, []byte(dshSession), ScanOptions{})
	if err != nil || s == nil {
		t.Fatalf("%v %v", s, err)
	}
	if s.SessionID != "session-11111111-2222-3333-4444-555555555555" || s.Cwd != "/home/dev/shop" || s.Title != "Fix order tests" {
		t.Fatalf("%+v", s)
	}
	if s.StartedAt == "" {
		t.Fatal("startedAt missing")
	}
	got := ir.Text(s.Render(0, -1))
	want := `USER: 用 pnpm 跑测试
ASSISTANT: 先跑一遍
  → bash(command=pnpm test)
  ✗ bash failed: connect ECONNREFUSED 127.0.0.1:5432
USER: 顺便看下 .env.test
ASSISTANT: 需要先启动数据库。
  [assistant turn aborted by the user]
  [assistant turn aborted by the user]
  [assistant error: boom]`
	if !strings.Contains(got, "[user ran /model deepseek-flash]") {
		t.Fatalf("slash command missing:\n%s", got)
	}
	idx := strings.Index(got, "USER: ")
	if idx < 0 || got[idx:] != want {
		t.Fatalf("got:\n%s\nwant tail:\n%s", got, want)
	}
	if strings.Contains(got, "hidden reasoning") || strings.Contains(got, "DeepSeek Harness") ||
		strings.Contains(got, "当前时间") || strings.Contains(got, "approval policy") {
		t.Fatal("thinking, the system prompt and injected context must be dropped")
	}
	starts := 0
	var failed *bool
	for _, l := range s.Render(0, -1) {
		if l.TurnStart {
			starts++
		}
		if l.Role == ir.RoleResult {
			failed = l.Success
		}
	}
	if starts != 2 {
		t.Fatalf("turn starts: %d", starts)
	}
	if failed == nil || *failed {
		t.Fatal("failed tool result not marked")
	}
}

func TestDshIncrementalOffsets(t *testing.T) {
	s, _ := Dsh{}.OpenBytes(ir.SourceFile{Kind: ir.Dsh, Path: "/x/session.v4.jsonl"}, []byte(dshSession), ScanOptions{})
	all := s.Render(0, -1)
	mid := all[len(all)/2].End
	head, tail := s.Render(0, mid), s.Render(mid, -1)
	if ir.Text(append(head, tail...)) != ir.Text(all) {
		t.Fatal("incremental render diverges")
	}
	if len(head) == 0 || len(tail) == 0 {
		t.Fatalf("split empty: %d %d", len(head), len(tail))
	}
}

func TestDshPartialLastLine(t *testing.T) {
	torn := dshSession + "{\"type\":\"user/message\",\"seq\":14,\"data\":{\"content\":[{\"type\":\"text\",\"text\":\"half\""
	s, err := Dsh{}.OpenBytes(ir.SourceFile{Kind: ir.Dsh, Path: "/x/session.v4.jsonl"}, []byte(torn), ScanOptions{})
	if err != nil || s == nil {
		t.Fatalf("%v %v", s, err)
	}
	if s.BadLines != 0 {
		t.Fatalf("a torn tail is not a bad line: %d", s.BadLines)
	}
	if strings.Contains(ir.Text(s.Render(0, -1)), "half") {
		t.Fatal("partial line rendered")
	}
	if s.CompleteBytes >= int64(len(torn)) {
		t.Fatalf("complete bytes %d cover the torn tail", s.CompleteBytes)
	}
}

func TestDshSniff(t *testing.T) {
	if !(Dsh{}).Sniff("", []byte(dshSession)) {
		t.Fatal("dsh header not sniffed")
	}
	omp := []byte(`{"type":"session","version":3,"id":"01a181a6","timestamp":"2026-10-01T00:00:00.000Z","cwd":"/x"}` + "\n")
	if (Dsh{}).Sniff("", omp) {
		t.Fatal("an omp header must not sniff as dsh")
	}
	// OMP's sniff only checks the record type, so a dsh header matches it too. Registry order (Dsh
	// before OMP) is what keeps them apart; this fails loudly if that assumption changes.
	if !(OMP{}).Sniff("", omp) || !(OMP{}).Sniff("", []byte(dshSession)) {
		t.Fatal("OMP sniff shape changed; recheck the registry order")
	}
	plain, zst := "/x/session.v4.jsonl", "/x/session.v4.jsonl.zstd"
	if dshKey(zst) != plain || dshKey(plain) != plain {
		t.Fatalf("key: %s %s", dshKey(zst), dshKey(plain))
	}
}

func TestDshDiscoverSubagents(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "--home-dev-shop--")
	write := func(id, header string) {
		dir := filepath.Join(proj, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := header + "\n" + `{"type":"user/message","seq":0,"time":1762000000000,"data":{"content":[{"type":"text","text":"hi"}],"source":{"kind":"user"},"role":"user","id":"m"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		// The live-session sidecar must not be picked up as a session.
		if err := os.WriteFile(filepath.Join(dir, "session.lock"), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("session-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		`{"type":"session","version":4,"id":"session-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","createdAt":1762000000000,"cwd":"/home/dev/shop","isSeeded":false,"delegationDepth":0}`)
	write("session-ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee",
		`{"type":"session","version":4,"id":"session-ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee","createdAt":1762000000000,"cwd":"/home/dev/shop","isSeeded":true,"delegationDepth":1,"origin":"subagent","parentSession":"session-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`)

	cfg := &config.Config{AgentDirs: map[ir.SourceKind][]string{ir.Dsh: {root}}}
	got := Dsh{}.Discover(cfg)
	if len(got) != 2 {
		t.Fatalf("discover: %d files", len(got))
	}
	var sub ir.SourceFile
	for _, f := range got {
		if strings.Contains(f.Path, "ffffffff") {
			sub = f
		}
		if !strings.HasSuffix(f.Key, ".jsonl") || strings.HasSuffix(f.Key, ".zstd") {
			t.Fatalf("key: %s", f.Key)
		}
	}
	if sub.ParentKey != "session-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" || sub.AgentName != "subagent" {
		t.Fatalf("subagent: %+v", sub)
	}
	cfg.IncludeSubagents = boolPtr(false)
	if got := (Dsh{}).Discover(cfg); len(got) != 1 {
		t.Fatalf("subagents off: %d", len(got))
	}
}

func TestDshCompressedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, enc.EncodeAll([]byte(dshSession), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := FileFor(path)
	if err != nil || f.Kind != ir.Dsh {
		t.Fatalf("FileFor: %+v %v", f, err)
	}
	if !strings.HasSuffix(f.Key, ".jsonl") || strings.HasSuffix(f.Key, ".zstd") {
		t.Fatalf("key keeps the plain name: %s", f.Key)
	}
	s, err := Open(f)
	if err != nil || s == nil || s.Title != "Fix order tests" {
		t.Fatalf("open compressed: %v %v", s, err)
	}
}

func TestDshExitCodeMarker(t *testing.T) {
	bashFail := "[stderr]\ncat: missing.txt: No such file or directory\n[exit code: 1]"
	tests := []struct {
		name, text string
		code       int
		ok         bool
	}{
		{"bash", bashFail, 1, true},
		{"bash", "all good\n[exit code: 0]", 0, true},
		{"bash", "no marker here", 0, false},
		{"bash", "[exit code: 1] echoed mid-output; report ends [exit code: 0]", 0, true},
		{"bash", "[exit code: 1] echoed mid-output then [exit code: 0] trailing", 0, false},
		// a marker quoted in the middle of the output is not the tool's report
		{"bash", "grep found: [exit code: 1] in a file\n[exit code: 0]", 0, true},
		{"bash", "a report the marker ends: [exit code: 1]", 1, true},
		// only shell tools: a read or grep result quoting the marker stays a success
		{"read", bashFail, 0, false},
		{"grep", bashFail, 0, false},
	}
	for _, tt := range tests {
		code, ok := dshExitCode(tt.name, tt.text)
		if code != tt.code || ok != tt.ok {
			t.Errorf("dshExitCode(%q, %q) = (%d, %v), want (%d, %v)", tt.name, tt.text, code, ok, tt.code, tt.ok)
		}
	}
}
