// Node-native test (node --test) for the alias editor's synthesis cache TTL
// field: days as the operator types them, seconds as the API stores them.
// Runs against the real functions extracted from app.js, the same technique
// as web/targetoptions.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const textMatch = src.match(/function synthCacheTTLText\(seconds\)[\s\S]*?\n}\n/);
assert.ok(textMatch, "synthCacheTTLText function not found in app.js");
const parseMatch = src.match(/function parseSynthCacheTTL\(text\)[\s\S]*?\n}\n/);
assert.ok(parseMatch, "parseSynthCacheTTL function not found in app.js");

const ctx = {};
vm.createContext(ctx);
vm.runInContext(textMatch[0] + "\n" + parseMatch[0] +
  "\nthis.synthCacheTTLText = synthCacheTTLText; this.parseSynthCacheTTL = parseSynthCacheTTL;", ctx);
const { synthCacheTTLText, parseSynthCacheTTL } = ctx;

test("synthCacheTTLText: the default (0 or missing) shows an empty field", () => {
  assert.equal(synthCacheTTLText(0), "");
  assert.equal(synthCacheTTLText(undefined), "");
});

test("synthCacheTTLText: stored seconds show as days", () => {
  assert.equal(synthCacheTTLText(172800), "2");
  assert.equal(synthCacheTTLText(129600), "1.5");
});

test("parseSynthCacheTTL: an empty field is the default", () => {
  assert.equal(parseSynthCacheTTL(""), 0);
  assert.equal(parseSynthCacheTTL("  "), 0);
});

test("parseSynthCacheTTL: days become whole seconds", () => {
  assert.equal(parseSynthCacheTTL("2"), 172800);
  assert.equal(parseSynthCacheTTL("0.5"), 43200);
  assert.equal(parseSynthCacheTTL("30"), 2592000);
});

test("parseSynthCacheTTL: out-of-range or non-numeric input is rejected", () => {
  for (const bad of ["-1", "0", "31", "week", "2d"]) {
    assert.throws(() => parseSynthCacheTTL(bad), /synthesis cache TTL/, bad);
  }
});
