// Node-native test (node --test) for small console helpers: the sidebar
// version label, money formatting, the UTC filter parsing, the review
// span finder, the tool-call finding note and the status badge. Same
// extraction technique as web/staleguard.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const grab = (name) => {
  const m = src.match(new RegExp(`function ${name}\\([^)]*\\)[\\s\\S]*?\\n}\\n`));
  assert.ok(m, `${name} not found in app.js`);
  return m[0];
};
const names = ["shortVersion", "fmtUSD", "utcInputToISO", "findSpan", "toolCallFindingNote", "statusBadge"];
const ctx = { TextEncoder };
vm.createContext(ctx);
vm.runInContext(names.map(grab).join("\n") + `\nObject.assign(this, { ${names.join(", ")} });`, ctx);

test("shortVersion: main builds shorten the sha, releases get a v", () => {
  assert.equal(ctx.shortVersion("main-9b1a93fc51f3734c5fbecf717d11ec641e94688d"), "main-9b1a93f");
  assert.equal(ctx.shortVersion("9b1a93fc51f3734c5fbecf717d11ec641e94688d"), "9b1a93f");
  assert.equal(ctx.shortVersion("0.1.86"), "v0.1.86");
  assert.equal(ctx.shortVersion("dev"), "dev");
  assert.equal(ctx.shortVersion(""), "dev");
});

test("fmtUSD: cents above a dollar, up to 4 decimals below, grouped", () => {
  assert.equal(ctx.fmtUSD(7.2), "$7.20");
  assert.equal(ctx.fmtUSD(312.0417), "$312.04");
  assert.equal(ctx.fmtUSD(12345.5), "$12,345.50");
  assert.equal(ctx.fmtUSD(0.1523), "$0.1523");
  assert.equal(ctx.fmtUSD(0), "$0.00");
});

test("utcInputToISO reads the datetime-local value as UTC", () => {
  assert.equal(ctx.utcInputToISO("2026-11-22T10:00"), "2026-11-22T10:00:00.000Z");
  assert.equal(ctx.utcInputToISO("2026-11-22T10:00:30"), "2026-11-22T10:00:30.000Z");
});

test("findSpan returns UTF-8 byte offsets within the first matching message", () => {
  const msgs = [{ content: "hello" }, { content: "ключ: sk-abc123" }];
  // "ключ: " is 4 two-byte letters + ": " = 10 bytes.
  assert.deepEqual({ ...ctx.findSpan(msgs, "sk-abc123") }, { start: 10, end: 19 });
  assert.equal(ctx.findSpan(msgs, "nope"), null);
});

test("toolCallFindingNote names the call and quotes its arguments", () => {
  const msgs = [
    { role: "user", content: "deploy" },
    { role: "assistant", tool_calls: [
      { id: "c0", function: { name: "read", arguments: "{}" } },
      { id: "c1", function: { name: "bash", arguments: '{"cmd":"export KEY=[REDACTED:openai_key]"}' } },
    ] },
  ];
  const n = ctx.toolCallFindingNote({ label: "openai_key", start: 0, end: 0, tool_call: { message: 1, index: 1, id: "c1" } }, msgs);
  assert.deepEqual({ ...n }, { where: "tool call bash (message 2)", excerpt: '{"cmd":"export KEY=[REDACTED:openai_key]"}' });

  const long = [{ tool_calls: [{ function: { name: "w", arguments: "x".repeat(200) } }] }];
  const ln = ctx.toolCallFindingNote({ tool_call: { message: 0, index: 0 } }, long);
  assert.equal(ln.excerpt, "x".repeat(160) + "…");

  // Body unavailable: falls back to the stored id, no excerpt.
  const gone = ctx.toolCallFindingNote({ tool_call: { message: 3, index: 0, id: "c9" } }, []);
  assert.deepEqual({ ...gone }, { where: "tool call c9 (message 4)", excerpt: "" });
});

test("statusBadge: 0 is neutral, 2xx green, errors red", () => {
  assert.match(ctx.statusBadge(0), /neutral/);
  assert.match(ctx.statusBadge(200), /active">200/);
  assert.match(ctx.statusBadge(502), /revoked">502/);
});
