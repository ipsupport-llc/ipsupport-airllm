// Node-native test (node --test) for staleGuard, the helper added to close
// Frontend I2: an out-of-order async response overwriting a view that a
// LATER call already rendered. Runs with zero dependencies (Node's built-in
// test runner) against the real function extracted from app.js, not a
// reimplementation — app.js itself isn't require()-able as a module (it's a
// plain browser script that touches `document`/`location` at top level), so
// the function source is extracted and evaluated in isolation.
//
// Lives OUTSIDE web/static/ deliberately: that whole directory is
// //go:embed all:static'd into the served SPA (see web/embed.go) — a test
// file sitting next to app.js would ship inside the binary and become a
// publicly fetchable static asset.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const match = src.match(/function staleGuard\(\)[\s\S]*?\n}\n/);
assert.ok(match, "staleGuard function not found in app.js");
const ctx = {};
vm.createContext(ctx);
vm.runInContext(match[0] + "\nthis.staleGuard = staleGuard;", ctx);
const { staleGuard } = ctx;

test("staleGuard: starting a new call invalidates the previous token", () => {
  const g = staleGuard();
  const t1 = g.start();
  assert.equal(g.current(t1), true);
  const t2 = g.start();
  assert.equal(g.current(t1), false, "the old token must no longer be current");
  assert.equal(g.current(t2), true);
});

// This is the exact shape of viewDashboard/viewUsage/loadKeys/loadCaptures:
// start a token, await a fetch, check current() before writing the DOM.
test("staleGuard closes the out-of-order-resolution race end to end", async () => {
  const g = staleGuard();
  let rendered = null;

  async function load(label, delay) {
    const token = g.start();
    await delay; // stand-in for the async fetch
    if (!g.current(token)) return; // the fix: drop a superseded response
    rendered = label; // stand-in for the DOM write
  }

  let resolveA;
  const aDelay = new Promise((r) => { resolveA = r; });
  const callA = load("A (stale)", aDelay); // starts first
  const callB = load("B (fresh)", Promise.resolve()); // starts second, resolves immediately
  await callB;
  assert.equal(rendered, "B (fresh)", "B must have rendered first");

  resolveA(); // A's response finally arrives, AFTER B already rendered
  await callA;
  assert.equal(rendered, "B (fresh)", "A's late, stale response must not overwrite B's fresher render");
});
