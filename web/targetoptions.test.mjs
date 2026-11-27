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

const extra = ["msToSecondsText", "parseSecondsToMs", "targetOptionsForm", "targetOptionsFromForm"].map((name) => {
  const m = src.match(new RegExp(`function ${name}\\([\\s\\S]*?\\n}\\n`));
  assert.ok(m, `${name} function not found in app.js`);
  return m[0] + `\nthis.${name} = ${name};\n`;
});

const ctx = {};
vm.createContext(ctx);
vm.runInContext(textMatch[0] + "\n" + parseMatch[0] +
  "\nthis.targetOptionsText = targetOptionsText; this.parseTargetOptions = parseTargetOptions;\n" + extra.join("\n"), ctx);
const { targetOptionsText, parseTargetOptions, msToSecondsText, parseSecondsToMs, targetOptionsForm, targetOptionsFromForm } = ctx;
// Objects built inside the vm context have another realm's prototype; a JSON
// round trip makes them comparable with deepEqual.
const plain = (o) => JSON.parse(JSON.stringify(o));

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

test("msToSecondsText: unset is empty, 0 stays 0, decimals are kept", () => {
  assert.equal(msToSecondsText(undefined), "");
  assert.equal(msToSecondsText(null), "");
  assert.equal(msToSecondsText(0), "0");
  assert.equal(msToSecondsText(1500), "1.5");
  assert.equal(msToSecondsText(60000), "60");
  assert.equal(msToSecondsText(1234), "1.234");
});

test("parseSecondsToMs: empty is unset, seconds become whole milliseconds", () => {
  assert.equal(parseSecondsToMs("", "timeout"), undefined);
  assert.equal(parseSecondsToMs("  ", "timeout"), undefined);
  assert.equal(parseSecondsToMs("0", "timeout"), 0);
  assert.equal(parseSecondsToMs("1.5", "timeout"), 1500);
  assert.equal(parseSecondsToMs("1.1", "timeout"), 1100);
  assert.equal(parseSecondsToMs(" 30 ", "timeout"), 30000);
  assert.equal(parseSecondsToMs("0.001", "timeout"), 1);
});

test("parseSecondsToMs: bad input is refused with the field's name", () => {
  for (const bad of ["abc", "-1", "1,5", "Infinity", "1.5s", "0x10", "1e3", "+5", ".5", "5.", "1 2"]) {
    assert.throws(() => parseSecondsToMs(bad, "timeout"), /^Error: timeout must be a number of seconds/, `accepted ${bad}`);
  }
  assert.equal(parseSecondsToMs("86400", "timeout"), 86400000);
  assert.throws(() => parseSecondsToMs("86400.5", "timeout"), /^Error: timeout must be at most 86400 seconds \(24 hours\)/);
  assert.throws(() => parseSecondsToMs("100000", "breaker cooldown", true), /breaker cooldown must be at most 86400 seconds/);
  assert.throws(() => parseSecondsToMs("0.0001", "timeout"), /at least 0.001/);
  assert.throws(() => parseSecondsToMs("0", "breaker cooldown", true), /breaker cooldown must be a number of seconds above 0/);
});

const blank = { timeout: "", fallback_on_auth: "", thinking: "", breaker_enabled: "", breaker_failures: "", breaker_cooldown: "", advanced: "" };

test("targetOptionsForm: known keys go to fields, the rest stays in Advanced", () => {
  const f = targetOptionsForm({
    timeout_ms: 1500, fallback_on_auth: false, thinking: "off",
    breaker: { enabled: true, failures: 3, cooldown_ms: 60000, error_rate: 0.5 },
    voices: { "en-US-Neural2-F": { voice: "nova" } }, later_key: [1, 2],
  });
  assert.equal(f.timeout, "1.5");
  assert.equal(f.fallback_on_auth, "no");
  assert.equal(f.thinking, "off");
  assert.equal(f.breaker_enabled, "yes");
  assert.equal(f.breaker_failures, "3");
  assert.equal(f.breaker_cooldown, "60");
  assert.deepEqual(JSON.parse(f.advanced), {
    breaker: { error_rate: 0.5 }, voices: { "en-US-Neural2-F": { voice: "nova" } }, later_key: [1, 2],
  });
});

test("targetOptionsForm: nothing set is all-empty fields", () => {
  assert.deepEqual(plain(targetOptionsForm(undefined)), blank);
  assert.deepEqual(plain(targetOptionsForm({})), blank);
});

test("round trip: form and back loses or changes no key", () => {
  const cases = [
    {},
    { timeout_ms: 0 },
    { timeout_ms: 1234, fallback_on_auth: true },
    { fallback_on_auth: false, breaker: { enabled: false } },
    { thinking: "off", breaker: { failures: 5, cooldown_ms: 1500, window_ms: 60000, min_requests: 10, max_cooldown_ms: 600000, stable_ms: 300000 } },
    {
      timeout_ms: 2000, recognition_models: { "en-US": "telephony" },
      voices: { a: { voice: "onyx", model: "tts-1", language: "en-US", gender: "male" } },
      default_voices: { en: { voice: "onyx" } }, send_language: true, stream_speech: false,
      breaker: { error_rate: 0.25 }, some_future_key: { nested: [1, { x: null }] },
    },
  ];
  for (const o of cases) {
    assert.deepEqual(plain(targetOptionsFromForm(targetOptionsForm(o))), o, JSON.stringify(o));
  }
});

test("targetOptionsFromForm: unset fields are omitted, not sent as null or 0", () => {
  assert.equal(JSON.stringify(targetOptionsFromForm(blank)), "{}");
});

test("targetOptionsFromForm: tri-state selects map to unset / true / false", () => {
  assert.equal("fallback_on_auth" in targetOptionsFromForm({ ...blank, fallback_on_auth: "" }), false);
  assert.equal(targetOptionsFromForm({ ...blank, fallback_on_auth: "yes" }).fallback_on_auth, true);
  assert.equal(targetOptionsFromForm({ ...blank, fallback_on_auth: "no" }).fallback_on_auth, false);
  assert.equal("breaker" in targetOptionsFromForm({ ...blank, breaker_enabled: "" }), false);
  assert.deepEqual(plain(targetOptionsFromForm({ ...blank, breaker_enabled: "no" })), { breaker: { enabled: false } });
  assert.deepEqual(plain(targetOptionsFromForm({ ...blank, breaker_enabled: "yes" })), { breaker: { enabled: true } });
});

test("targetOptionsFromForm: fields merge with Advanced, which keeps its other keys", () => {
  const o = targetOptionsFromForm({
    ...blank, timeout: "2.5", breaker_failures: "4",
    advanced: '{"breaker":{"error_rate":0.5},"voices":{"x":{"voice":"y"}}}',
  });
  assert.deepEqual(plain(o), { timeout_ms: 2500, breaker: { failures: 4, error_rate: 0.5 }, voices: { x: { voice: "y" } } });
});

test("targetOptionsFromForm: a key in Advanced with its field empty is kept", () => {
  const o = targetOptionsFromForm({ ...blank, advanced: '{"timeout_ms":9000,"thinking":"off","breaker":{"failures":2}}' });
  assert.deepEqual(plain(o), { timeout_ms: 9000, thinking: "off", breaker: { failures: 2 } });
});

test("targetOptionsFromForm: a key set in its field and in Advanced is refused, naming the key", () => {
  const cases = [
    [{ timeout: "2" }, '{"timeout_ms":9000}', "timeout_ms"],
    [{ fallback_on_auth: "no" }, '{"fallback_on_auth":true}', "fallback_on_auth"],
    [{ thinking: "off" }, '{"thinking":"off"}', "thinking"],
    [{ breaker_enabled: "yes" }, '{"breaker":{"enabled":false}}', "breaker.enabled"],
    [{ breaker_failures: "4" }, '{"breaker":{"failures":1}}', "breaker.failures"],
    [{ breaker_cooldown: "30" }, '{"breaker":{"cooldown_ms":1000}}', "breaker.cooldown_ms"],
  ];
  for (const [fields, advanced, key] of cases) {
    assert.throws(() => targetOptionsFromForm({ ...blank, ...fields, advanced }),
      new RegExp(`^Error: ${key.replace(".", "\\.")} is set both in its field and in Advanced \\(JSON\\)`), key);
  }
});

test("targetOptionsForm: a stored value its field would refuse stays in Advanced", () => {
  const stored = { timeout_ms: 90000000, breaker: { failures: 0, cooldown_ms: 0, enabled: true } };
  const f = targetOptionsForm(stored);
  assert.equal(f.timeout, "");
  assert.equal(f.breaker_failures, "");
  assert.equal(f.breaker_cooldown, "");
  assert.equal(f.breaker_enabled, "yes");
  assert.deepEqual(JSON.parse(f.advanced), { timeout_ms: 90000000, breaker: { failures: 0, cooldown_ms: 0 } });
  // An unrelated edit still saves, and the stored values go back untouched
  // for the server to judge.
  assert.deepEqual(plain(targetOptionsFromForm({ ...f, fallback_on_auth: "yes" })), { ...stored, fallback_on_auth: true });
});

test("targetOptionsFromForm: bad input names the field", () => {
  assert.throws(() => targetOptionsFromForm({ ...blank, timeout: "fast" }), /timeout must be a number of seconds/);
  for (const bad of ["0", "-2", "2.5", "x"]) {
    assert.throws(() => targetOptionsFromForm({ ...blank, breaker_failures: bad }), /breaker failures must be a whole number above 0/, `accepted ${bad}`);
  }
  assert.throws(() => targetOptionsFromForm({ ...blank, breaker_cooldown: "0" }), /breaker cooldown must be/);
  assert.throws(() => targetOptionsFromForm({ ...blank, advanced: "{oops" }), /^Error: Advanced \(JSON\): options must be valid JSON/);
  assert.throws(() => targetOptionsFromForm({ ...blank, breaker_failures: "3", advanced: '{"breaker":5}' }), /Advanced \(JSON\): breaker must be a JSON object/);
});
