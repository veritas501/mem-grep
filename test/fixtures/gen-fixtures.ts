// Regenerate test fixtures with omp's OWN SessionManager, so they match what omp really writes.
// Usage (in a scratch dir, not in this repo):
//   bun add @oh-my-pi/pi-coding-agent
//   HOME=$PWD/fakehome bun /path/to/mem-grep/test/fixtures/gen-fixtures.ts $PWD/out
// then copy out/-projects-shop-api/<ts>_<id>.jsonl -> test/fixtures/sessions/-projects-shop-api/main.jsonl
// and its sibling directory -> .../main/ (contains scout.jsonl). Tests assert entry ids such as #1a38e318
// and counts, so update the Go tests (internal/sources, internal/ingest, internal/transcripts) if the fixture changes.
import { mkdirSync, renameSync } from "fs";
import { join } from "path";
const smPath = process.env.OMP_SESSION_MANAGER ?? join(process.cwd(), "node_modules/@oh-my-pi/pi-coding-agent/src/session/session-manager.ts");
const { SessionManager } = await import(smPath);

const out = process.argv[2];
const cwd = "/home/dev/projects/shop-api";
const dir = join(out, "-projects-shop-api");
mkdirSync(dir, { recursive: true });

let t = Date.parse("2026-10-20T09:00:00Z");
const ts = () => (t += 7000);
const usage = { input: 100, output: 50, cacheRead: 0, cacheWrite: 0, totalTokens: 150, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
const asst = (content: any[], stopReason = "stop") => ({ role: "assistant", content, api: "anthropic-messages", provider: "anthropic", model: "claude-sonnet-4-6", usage, stopReason, timestamp: ts() });
const user = (text: string) => ({ role: "user", content: text, timestamp: ts() });
const tr = (id: string, name: string, text: string, isError = false) => ({ role: "toolResult", toolCallId: id, toolName: name, content: [{ type: "text", text }], isError, timestamp: ts() });

const sm = SessionManager.create(cwd, dir);
sm.appendMessage(user("帮我把订单服务的单元测试跑通，用 pnpm，不要用 npm。") as any);
sm.appendMessage(asst([{ type: "thinking", thinking: "secret thoughts" }, { type: "text", text: "好的，先看看测试配置。" }, { type: "toolCall", id: "c1", name: "bash", arguments: { command: "pnpm test --filter order" } }], "toolUse") as any);
sm.appendMessage(tr("c1", "bash", "Error: connect ECONNREFUSED 127.0.0.1:5432\n    at TCPConnectWrap.afterConnect", true) as any);
const beforeRetry = sm.appendMessage(asst([{ type: "text", text: "测试需要本地 Postgres。我尝试 mock 掉数据库。" }, { type: "toolCall", id: "c2", name: "edit", arguments: { path: "src/order/order.test.ts", old_text: "x", new_text: "y" } }], "toolUse") as any);
sm.appendMessage(tr("c2", "edit", "ok") as any);
sm.appendMessage(user("别 mock 数据库！我们一直用 docker compose 起 test-db。") as any);
// user rewinds to before the mocking attempt
const branchPoint = sm.getEntries().find((e: any) => e.message?.role === "toolResult" && e.message.toolCallId === "c1")!.id;
sm.branchWithSummary(branchPoint, "Tried mocking the DB in order.test.ts; user rejected it.");
sm.appendMessage(asst([{ type: "toolCall", id: "c3", name: "bash", arguments: { command: "docker compose -f docker-compose.test.yml up -d test-db && pnpm test --filter order" } }], "toolUse") as any);
sm.appendMessage(tr("c3", "bash", "Tests: 42 passed, 42 total\nDone in 8.1s") as any);
sm.appendMessage(asst([{ type: "text", text: "全部通过。注意 test-db 用的是 5433 端口，配置在 .env.test，API key 是 sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123。" }]) as any);
sm.appendMessage({ role: "bashExecution", command: "git status --short", output: " M src/order/order.service.ts", exitCode: 0, cancelled: false, truncated: false, timestamp: ts() } as any);
sm.appendResetBoundary();
sm.appendMessage(user("顺便把 README 里的启动说明改成中文。") as any);
sm.appendMessage(asst([{ type: "text", text: "已更新 README。" }], "aborted") as any);
await sm.flush();
const file = sm.getSessionFile()!;
console.log("main:", file);

// subagent in sibling directory named after the session file
const subDir = file.replace(/\.jsonl$/, "");
mkdirSync(subDir, { recursive: true });
const sub = SessionManager.create(cwd, subDir);
sub.appendSessionInit({ systemPrompt: ["You are a scout."], task: "找出 order.service.ts 中所有未处理的 Promise", tools: ["read", "search"], agent: "scout" } as any);
sub.appendMessage(asst([{ type: "toolCall", id: "s1", name: "search", arguments: { pattern: "\\.then\\(", path: "src/order" } }], "toolUse") as any);
sub.appendMessage(tr("s1", "search", "src/order/order.service.ts:88") as any);
sub.appendMessage(asst([{ type: "text", text: "第 88 行的 .then 没有 catch。" }]) as any);
await sub.flush();
const subFile = sub.getSessionFile()!;
renameSync(subFile, join(subDir, "scout.jsonl"));
console.log("sub:", join(subDir, "scout.jsonl"));
