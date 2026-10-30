#!/usr/bin/env node
// A thin translator from the Gemini generateContent protocol to an OpenAI-compatible chat API, so Gemini
// CLI can talk to a model that has no Gemini endpoint (DeepSeek). It exists for test/fixtures/real/gen.sh
// only. The API key is read from the environment of this process (UPSTREAM_API_KEY) and never written
// anywhere; requests and replies are not logged, only their routes.
//
//   UPSTREAM_API_KEY=… node gemini-to-openai.mjs <port> <upstream base url> <upstream model>

import http from "node:http";

const [portArg, base = "https://api.deepseek.com/v1", model = "deepseek-flash"] = process.argv.slice(2);
const port = Number(portArg ?? 18090);
const key = process.env.UPSTREAM_API_KEY;
if (!key) { console.error("UPSTREAM_API_KEY is not set"); process.exit(2); }

// Gemini schemas may use upper-case type names; JSON Schema wants lower case
const lowerTypes = (s) => Array.isArray(s) ? s.map(lowerTypes) : s && typeof s === "object"
  ? Object.fromEntries(Object.entries(s).map(([k, v]) => [k, k === "type" && typeof v === "string" ? v.toLowerCase() : lowerTypes(v)])) : s;

function toOpenAI(body) {
  const messages = [];
  const sys = (body.systemInstruction?.parts ?? []).map((p) => p.text ?? "").join("\n");
  const json = body.generationConfig?.responseMimeType === "application/json";
  const schema = body.generationConfig?.responseJsonSchema ?? body.generationConfig?.responseSchema;
  const system = [sys, json ? "Respond with a single JSON object only." + (schema ? " It must match this JSON schema: " + JSON.stringify(lowerTypes(schema)) : "") : ""].filter(Boolean).join("\n\n");
  if (system) messages.push({ role: "system", content: system });
  let counter = 0;
  const pending = {}; // tool name -> ids of calls still waiting for their response
  for (const c of body.contents ?? []) {
    const parts = c.parts ?? [];
    if (c.role === "model") {
      const text = parts.filter((p) => p.text !== undefined && !p.thought).map((p) => p.text).join("");
      const calls = parts.filter((p) => p.functionCall).map((p) => {
        const id = p.functionCall.id || `call_${++counter}`;
        (pending[p.functionCall.name] ??= []).push(id);
        return { id, type: "function", function: { name: p.functionCall.name, arguments: JSON.stringify(p.functionCall.args ?? {}) } };
      });
      if (text || calls.length) messages.push({ role: "assistant", content: text || null, ...(calls.length ? { tool_calls: calls } : {}) });
    } else {
      for (const p of parts.filter((p) => p.functionResponse)) {
        const fr = p.functionResponse;
        const id = fr.id && Object.values(pending).some((l) => l.includes(fr.id)) ? fr.id : (pending[fr.name] ?? []).shift() ?? `call_${++counter}`;
        for (const l of Object.values(pending)) { const i = l.indexOf(id); if (i >= 0) l.splice(i, 1); }
        messages.push({ role: "tool", tool_call_id: id, content: typeof fr.response === "string" ? fr.response : JSON.stringify(fr.response ?? {}) });
      }
      const text = parts.filter((p) => p.text !== undefined).map((p) => p.text).join("\n");
      if (text) messages.push({ role: "user", content: text });
    }
  }
  const tools = (body.tools ?? []).flatMap((t) => t.functionDeclarations ?? []).map((d) => ({
    type: "function", function: { name: d.name, description: d.description ?? "", parameters: lowerTypes(d.parametersJsonSchema ?? d.parameters ?? { type: "object", properties: {} }) },
  }));
  return { model, messages, ...(tools.length ? { tools } : {}), ...(json ? { response_format: { type: "json_object" } } : {}), stream: false, thinking: { type: "disabled" } };
}

function toGemini(reply) {
  const msg = reply.choices?.[0]?.message ?? {};
  const parts = [];
  if (msg.content) parts.push({ text: msg.content });
  for (const c of msg.tool_calls ?? []) {
    let args = {};
    try { args = JSON.parse(c.function.arguments || "{}"); } catch {}
    parts.push({ functionCall: { id: c.id, name: c.function.name, args } });
  }
  if (!parts.length) parts.push({ text: "" });
  const u = reply.usage ?? {};
  return { candidates: [{ content: { role: "model", parts }, finishReason: "STOP", index: 0 }], usageMetadata: { promptTokenCount: u.prompt_tokens ?? 0, candidatesTokenCount: u.completion_tokens ?? 0, totalTokenCount: u.total_tokens ?? 0 }, modelVersion: model };
}

http.createServer((req, res) => {
  let raw = "";
  req.on("data", (d) => (raw += d));
  req.on("end", async () => {
    const url = req.url.split("?")[0];
    const reply = (code, obj) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(obj)); };
    try {
      if (/:countTokens$/.test(url)) return reply(200, { totalTokens: 100 });
      const stream = /:streamGenerateContent$/.test(url);
      if (!stream && !/:generateContent$/.test(url)) return reply(404, { error: { code: 404, message: "no route " + url, status: "NOT_FOUND" } });
      const up = await fetch(base + "/chat/completions", { method: "POST", headers: { "content-type": "application/json", authorization: "Bearer " + key }, body: JSON.stringify(toOpenAI(JSON.parse(raw || "{}"))) });
      const text = await up.text();
      console.error(`${stream ? "stream" : "generate"} -> upstream ${up.status}`);
      if (!up.ok) return reply(up.status, { error: { code: up.status, message: text.slice(0, 300), status: "UPSTREAM_ERROR" } });
      const out = toGemini(JSON.parse(text));
      if (!stream) return reply(200, out);
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      res.end(`data: ${JSON.stringify(out)}\r\n\r\n`);
    } catch (e) {
      reply(500, { error: { code: 500, message: String(e), status: "INTERNAL" } });
    }
  });
}).listen(port, "127.0.0.1", () => console.error(`gemini -> ${base} (${model}) on http://127.0.0.1:${port}`));
