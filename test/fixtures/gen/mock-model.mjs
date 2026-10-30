#!/usr/bin/env node
// A scripted stand-in for a model API, used to make coding agents write session logs without a real
// model or any credentials. It speaks the OpenAI chat-completions protocol (opencode, Qwen Code, …)
// and the Gemini generateContent protocol (Gemini CLI). The reply depends only on the request:
// the last user message names a scenario ("SCENARIO list-files …") and the number of tool results
// since then picks the step. Tool names and argument names come from the tools the agent offers,
// so one script serves every agent. Everything else (titles, summaries, classifiers) gets a short
// plain reply.
//
//   node mock-model.mjs [port] [--log file]      (default port 18080)
//
// Scenarios (the agent really runs the tools it is asked to, in a temporary directory that holds
// notes.txt):
//   list-files  text + shell `ls -la`, read notes.txt, final text
//   recover     shell `cat missing.txt` (fails), text + shell `echo recovered`, final text
//   followup    final text only (a second turn of a resumed session)
//   delegate    text + the task tool (a subagent session that plays `followup`), final text

import http from "node:http";
import fs from "node:fs";

const args = process.argv.slice(2);
const port = Number(args.find((a) => /^\d+$/.test(a)) ?? 18080);
const logFile = args.includes("--log") ? args[args.indexOf("--log") + 1] : null;
const log = (o) => logFile && fs.appendFileSync(logFile, JSON.stringify(o) + "\n");

const SCENARIOS = {
  "list-files": [
    { text: "我先看看目录里有什么。", tool: ["shell", { command: "ls -la" }] },
    { tool: ["read", { path: "notes.txt" }] },
    { text: "目录里有 notes.txt，内容说明 pnpm test 因 ECONNREFUSED 127.0.0.1:5432 失败，需要先启动 test-db。" },
  ],
  recover: [
    { tool: ["shell", { command: "cat missing.txt" }] },
    { text: "missing.txt 不存在，改用 echo 确认环境正常。", tool: ["shell", { command: "echo recovered" }] },
    { text: "已恢复：环境正常，不需要 missing.txt。" },
  ],
  followup: [{ text: "好的，不需要更多改动。" }],
  delegate: [
    { text: "交给子任务去看看。", tool: ["task", { description: "look around", prompt: "SCENARIO followup: look around", subagent_type: "general" }] },
    { text: "子任务完成了。" },
  ],
};

// pick the offered tool that fits a role and fill its arguments from its schema
const TOOL_NAME = { shell: /bash|shell|exec|run_command/i, read: /^read|read_file|readfile/i, task: /^(task|agent|invoke_agent)$/i };
function chooseTool(tools, kind, wanted) {
  const tool = tools.find((t) => TOOL_NAME[kind].test(t.name));
  if (!tool) return null;
  const props = Object.keys(tool.schema?.properties ?? {});
  const out = {};
  for (const [k, v] of Object.entries(wanted)) {
    const key = kind === "shell" ? props.find((p) => /^(command|cmd)$/i.test(p)) ?? props.find((p) => /command/i.test(p))
      : kind === "read" ? props.find((p) => /path/i.test(p) || /file/i.test(p))
      : props.includes(k) ? k : props.find((p) => p.toLowerCase().includes(k.split("_")[0]));
    out[key ?? k] = k === "path" && process.env.MOCK_WORKDIR ? `${process.env.MOCK_WORKDIR}/${v}` : v;
  }
  if (props.includes("agent_name")) out.agent_name = "generalist"; // Gemini CLI's invoke_agent
  // required properties the script does not know (e.g. a description) get a short text
  for (const r of tool.schema?.required ?? []) if (!(r in out)) out[r] = "run";
  return { name: tool.name, args: out };
}

function plan(userText, steps, tools) {
  const m = /SCENARIO ([\w-]+)/.exec(userText ?? "");
  const script = m && SCENARIOS[m[1]];
  if (!script) return { text: "ok" };
  const step = script[Math.min(steps, script.length - 1)];
  const call = step.tool ? chooseTool(tools, step.tool[0], step.tool[1]) : null;
  return { text: step.text, call };
}

let nextId = 1;
const id = (p) => `${p}_${String(nextId++).padStart(4, "0")}`;

// ---- OpenAI chat completions ---------------------------------------------------------------
function openai(req, body, res) {
  const msgs = body.messages ?? [];
  const text = (c) => (typeof c === "string" ? c : (c ?? []).map((p) => p.text ?? "").join("\n"));
  let last = -1;
  msgs.forEach((m, i) => { if (m.role === "user") last = i; });
  const steps = msgs.slice(last + 1).filter((m) => m.role === "tool").length;
  const tools = (body.tools ?? []).map((t) => ({ name: t.function.name, schema: t.function.parameters }));
  const p = plan(last >= 0 ? text(msgs[last].content) : "", steps, tools);
  const model = body.model ?? "mock";
  const created = Math.floor(Date.now() / 1000);
  const cid = id("chatcmpl");
  const usage = { prompt_tokens: 100 + steps * 50, completion_tokens: 20, total_tokens: 120 + steps * 50 };
  const toolCalls = p.call ? [{ id: id("call"), type: "function", function: { name: p.call.name, arguments: JSON.stringify(p.call.args) } }] : undefined;
  const finish = toolCalls ? "tool_calls" : "stop";
  if (!body.stream) {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ id: cid, object: "chat.completion", created, model, choices: [{ index: 0, message: { role: "assistant", content: p.text ?? null, tool_calls: toolCalls }, finish_reason: finish }], usage }));
    return;
  }
  res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
  const send = (delta, fr = null, u) => res.write(`data: ${JSON.stringify({ id: cid, object: "chat.completion.chunk", created, model, choices: [{ index: 0, delta, finish_reason: fr }], ...(u ? { usage: u } : {}) })}\n\n`);
  send({ role: "assistant", content: "" });
  if (p.text) for (const piece of p.text.match(/.{1,8}/gs)) send({ content: piece });
  if (toolCalls) send({ tool_calls: [{ index: 0, ...toolCalls[0], function: { name: toolCalls[0].function.name, arguments: "" } }] }), send({ tool_calls: [{ index: 0, function: { arguments: toolCalls[0].function.arguments } }] });
  send({}, finish);
  if (body.stream_options?.include_usage) res.write(`data: ${JSON.stringify({ id: cid, object: "chat.completion.chunk", created, model, choices: [], usage })}\n\n`);
  res.end("data: [DONE]\n\n");
}

// ---- Gemini generateContent ----------------------------------------------------------------
function gemini(req, body, res, stream) {
  const contents = body.contents ?? [];
  const textOf = (c) => (c.parts ?? []).filter((p) => p.text !== undefined).map((p) => p.text).join("\n");
  const isResponse = (c) => (c.parts ?? []).some((p) => p.functionResponse);
  let last = -1;
  contents.forEach((c, i) => { if (c.role === "user" && !isResponse(c) && textOf(c)) last = i; });
  const steps = contents.slice(last + 1).filter(isResponse).length;
  const tools = (body.tools ?? []).flatMap((t) => t.functionDeclarations ?? []).map((d) => ({ name: d.name, schema: d.parameters ?? d.parametersJsonSchema }));
  // "tools: none" requests (classifiers, summaries) get a plain reply; JSON requests an empty object
  const wantsJson = body.generationConfig?.responseMimeType === "application/json";
  const p = wantsJson ? { text: "{}" } : plan(last >= 0 ? textOf(contents[last]) : "", steps, tools);
  const parts = [];
  if (p.text) parts.push({ text: p.text });
  if (p.call) parts.push({ functionCall: { name: p.call.name, args: p.call.args } });
  const out = { candidates: [{ content: { role: "model", parts }, finishReason: "STOP", index: 0 }], usageMetadata: { promptTokenCount: 100 + steps * 50, candidatesTokenCount: 20, totalTokenCount: 120 + steps * 50 }, modelVersion: "mock" };
  if (!stream) { res.writeHead(200, { "content-type": "application/json" }); res.end(JSON.stringify(out)); return; }
  res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
  res.end(`data: ${JSON.stringify(out)}\r\n\r\n`);
}

http.createServer((req, res) => {
  let raw = "";
  req.on("data", (d) => (raw += d));
  req.on("end", () => {
    let body = {};
    try { body = raw ? JSON.parse(raw) : {}; } catch {}
    const url = req.url.split("?")[0];
    const toolNames = (body.tools ?? []).map((t) => t.function?.name ?? t.functionDeclarations?.map((d) => d.name) ?? t.name).flat();
    log({ method: req.method, url, keys: Object.keys(body), tools: toolNames });
    try {
      if (req.method === "GET" && /\/models\/?$/.test(url)) {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ object: "list", data: [{ id: "mock-1", object: "model", created: 0, owned_by: "mock" }], models: [] }));
      } else if (/\/chat\/completions$/.test(url)) openai(req, body, res);
      else if (/:streamGenerateContent$/.test(url)) gemini(req, body, res, true);
      else if (/:generateContent$/.test(url)) gemini(req, body, res, false);
      else if (/:countTokens$/.test(url)) { res.writeHead(200, { "content-type": "application/json" }); res.end(JSON.stringify({ totalTokens: 100 })); }
      else { res.writeHead(404, { "content-type": "application/json" }); res.end(JSON.stringify({ error: { message: "mock: no route " + url } })); }
    } catch (e) {
      res.writeHead(500, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: { message: String(e) } }));
    }
  });
}).listen(port, "127.0.0.1", () => console.error(`mock model on http://127.0.0.1:${port}`));
