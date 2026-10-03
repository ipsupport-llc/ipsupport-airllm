// Node-native test (node --test) for the alias editor's session pin TTL
// field: hours as the operator types them, seconds as the API stores them.
// Runs against the real functions extracted from app.js, the same technique
// as web/targetoptions.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const textMatch = src.match(/function affinityTTLText\(seconds\)[\s\S]*?\n}\n/);
assert.ok(textMatch, "affinityTTLText function not found in app.js");
const parseMatch = src.match(/function parseAffinityTTL\(text\)[\s\S]*?\n}\n/);
assert.ok(parseMatch, "parseAffinityTTL function not found in app.js");

const ctx = {};
vm.createContext(ctx);
vm.runInContext(textMatch[0] + "\n" + parseMatch[0] +
  "\nthis.affinityTTLText = affinityTTLText; this.parseAffinityTTL = parseAffinityTTL;", ctx);
const { affinityTTLText, parseAffinityTTL } = ctx;

test("affinityTTLText: the default (0 or missing) shows an empty field", () => {
  assert.equal(affinityTTLText(0), "");
  assert.equal(affinityTTLText(undefined), "");
});

test("affinityTTLText: stored seconds show as hours", () => {
  assert.equal(affinityTTLText(7200), "2");
  assert.equal(affinityTTLText(5400), "1.5");
});

test("parseAffinityTTL: an empty field is the default", () => {
  assert.equal(parseAffinityTTL(""), 0);
  assert.equal(parseAffinityTTL("  "), 0);
});

test("parseAffinityTTL: hours become whole seconds", () => {
  assert.equal(parseAffinityTTL("2"), 7200);
  assert.equal(parseAffinityTTL("0.25"), 900);
});

test("parseAffinityTTL: out-of-range or non-numeric input is rejected", () => {
  for (const bad of ["-1", "0", "169", "four", "1h"]) {
    assert.throws(() => parseAffinityTTL(bad), /session pin TTL/, bad);
  }
});
