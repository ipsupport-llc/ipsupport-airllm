// Node-native test (node --test) for the alias editor's per-target options
// field: what it shows for a stored options object and what it sends back.
// Runs against the real functions extracted from app.js, the same technique
// as web/staleguard.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const textMatch = src.match(/function targetOptionsText\(o\)[\s\S]*?\n}\n/);
assert.ok(textMatch, "targetOptionsText function not found in app.js");
const parseMatch = src.match(/function parseTargetOptions\(text\)[\s\S]*?\n}\n/);
assert.ok(parseMatch, "parseTargetOptions function not found in app.js");

const ctx = {};
vm.createContext(ctx);
vm.runInContext(textMatch[0] + "\n" + parseMatch[0] +
  "\nthis.targetOptionsText = targetOptionsText; this.parseTargetOptions = parseTargetOptions;", ctx);
const { targetOptionsText, parseTargetOptions } = ctx;

test("targetOptionsText: an empty or missing object shows an empty field", () => {
  assert.equal(targetOptionsText({}), "");
  assert.equal(targetOptionsText(undefined), "");
  assert.equal(targetOptionsText(null), "");
});

test("targetOptionsText: a stored object shows as JSON, unknown keys included", () => {
  const shown = targetOptionsText({ timeout_ms: 1500, later_key: { x: 1 } });
  assert.deepEqual(JSON.parse(shown), { timeout_ms: 1500, later_key: { x: 1 } });
});

test("parseTargetOptions: an empty field is the empty object", () => {
  assert.equal(JSON.stringify(parseTargetOptions("  ")), "{}");
});

test("parseTargetOptions: an object comes back as typed", () => {
  const o = parseTargetOptions('{"timeout_ms": 2000, "fallback_on_auth": true}');
  assert.equal(o.timeout_ms, 2000);
  assert.equal(o.fallback_on_auth, true);
});

test("parseTargetOptions: anything but a JSON object is refused", () => {
  for (const bad of ["{oops", "[1]", "null", '"fast"', "42"]) {
    assert.throws(() => parseTargetOptions(bad), /options/, `accepted ${bad}`);
  }
});
