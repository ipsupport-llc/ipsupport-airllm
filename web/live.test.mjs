// Node-native test (node --test) for the Live page's pure helpers:
// session grouping, elapsed/byte formatting and stale-pod detection. Runs
// against the real functions extracted from app.js, the same technique as
// web/staleguard.test.mjs (see its header for why this file lives outside
// web/static/).
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
const ctx = {};
vm.createContext(ctx);
vm.runInContext(["fmtElapsed", "fmtBytes", "groupLive", "stalePods"].map(grab).join("\n") +
  "\nObject.assign(this, { fmtElapsed, fmtBytes, groupLive, stalePods });", ctx);
const { fmtElapsed, fmtBytes, groupLive, stalePods } = ctx;

test("fmtElapsed: seconds, minutes, hours", () => {
  assert.equal(fmtElapsed(900), "0.9s");
  assert.equal(fmtElapsed(59999), "60.0s");
  assert.equal(fmtElapsed(65000), "1m 05s");
  assert.equal(fmtElapsed(2 * 3600e3 + 3 * 60e3), "2h 03m");
  assert.equal(fmtElapsed(-5), "0.0s");
});

test("fmtBytes", () => {
  assert.equal(fmtBytes(0), "0 B");
  assert.equal(fmtBytes(812), "812 B");
  assert.equal(fmtBytes(48211), "47.1 KB");
  assert.equal(fmtBytes(3.2 * 1024 * 1024), "3.2 MB");
});

test("groupLive: a session with 2+ requests becomes one group at its first request's position", () => {
  const reqs = [
    { id: "a", session_id: "s1" },
    { id: "b", session_id: "" },
    { id: "c", session_id: "s2" },
    { id: "d", session_id: "s1" },
  ];
  const g = groupLive(reqs);
  assert.deepEqual(JSON.parse(JSON.stringify(g.map((x) => [x.session_id, x.requests.map((r) => r.id)]))), [
    ["s1", ["a", "d"]],
    ["", ["b"]],
    ["", ["c"]], // a lone session request needs no header row
  ]);
  assert.equal(groupLive(null).length, 0); // (cross-realm array: compare by length)
});

test("stalePods: only pods whose heartbeat is older than the limit", () => {
  const at = "2026-11-22T10:00:00Z";
  const s = stalePods([
    { pod: "fresh", seen_at: "2026-11-22T09:59:58Z" },
    { pod: "old", seen_at: "2026-11-22T09:59:15Z" },
  ], at, 10000);
  assert.equal(s.length, 1);
  assert.equal(s[0].pod, "old");
  assert.equal(s[0].age_ms, 45000);
  assert.equal(stalePods([{ pod: "x", seen_at: at }], "garbage", 10000).length, 0);
});
