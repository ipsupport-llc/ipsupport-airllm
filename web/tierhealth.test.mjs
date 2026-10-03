// Node-native test (node --test) for the alias tier-health view: how each
// tier's breaker state from the health endpoint is shown, and which tiers
// offer a manual release. Runs against the real functions extracted from
// app.js, the same technique as web/targetoptions.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const escMatch = src.match(/function esc\(s\)[\s\S]*?\n}\n/);
assert.ok(escMatch, "esc function not found in app.js");
const rowsMatch = src.match(/function tierHealthRows\(tiers\)[\s\S]*?\n}\n/);
assert.ok(rowsMatch, "tierHealthRows function not found in app.js");

const ctx = {};
vm.createContext(ctx);
vm.runInContext(escMatch[0] + "\n" + rowsMatch[0] + "\nthis.tierHealthRows = tierHealthRows;", ctx);
const { tierHealthRows } = ctx;

const open = {
  tier: 0, breaker_enabled: true, state: "open", reason: "consecutive_failures",
  open_until: "2026-01-01T00:01:00Z", cooldown_ms: 120000,
  targets: [{ provider: "vertex", upstream_model: "gemini" }],
};

test("an open tier shows its reason, cooldown and a release button", () => {
  const [row] = tierHealthRows([open]);
  assert.match(row, /badge revoked">open</);
  assert.match(row, /consecutive_failures/);
  assert.match(row, /120s/);
  assert.match(row, /vertex\/gemini/);
  assert.match(row, /data-release="0"/);
});

test("a probing tier is releasable too", () => {
  const [row] = tierHealthRows([{ ...open, tier: 10, state: "half_open" }]);
  assert.match(row, />probing</);
  assert.match(row, /data-release="10"/);
});

test("a closed tier has no reason and no release button", () => {
  const [row] = tierHealthRows([{ ...open, state: "closed", reason: "", cooldown_ms: 0 }]);
  assert.match(row, /badge active">closed</);
  assert.doesNotMatch(row, /consecutive_failures/);
  assert.doesNotMatch(row, /data-release/);
});

test("a tier without a breaker shows it as off", () => {
  const [row] = tierHealthRows([{ ...open, breaker_enabled: false, state: "closed", reason: "", cooldown_ms: 0 }]);
  assert.match(row, />off</);
  assert.doesNotMatch(row, /data-release/);
});

test("target names are escaped", () => {
  const [row] = tierHealthRows([{ ...open, targets: [{ provider: "<b>", upstream_model: "m" }] }]);
  assert.match(row, /&lt;b&gt;\/m/);
});
