// Node-native test (node --test) for renderFormField, the modalForm field
// renderer. Covers the Frontend Minor fix: every field branch except the
// bare default fallback got native HTML5 validation (type="number" +
// min/max/step) on numeric fields elsewhere in this file — the generic
// fallback modalForm uses for fields like editProvider's max_concurrency
// and editPrice's rate fields had none, so a negative max_concurrency only
// surfaced as a generic "Failed" toast after a round trip to the backend
// (which itself rejects negative values — a separate Admin API fix earlier
// in this campaign). Runs with zero dependencies against the real function
// extracted from app.js, not a reimplementation — same technique as
// web/staleguard.test.mjs (see its header comment for why this file lives
// outside web/static/).
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");

const escMatch = src.match(/function esc\(s\)[\s\S]*?\n}\n/);
assert.ok(escMatch, "esc function not found in app.js");
const fieldMatch = src.match(/function renderFormField\(f\)[\s\S]*?\n}\n/);
assert.ok(fieldMatch, "renderFormField function not found in app.js");

const ctx = {};
vm.createContext(ctx);
vm.runInContext(escMatch[0] + "\n" + fieldMatch[0] + "\nthis.renderFormField = renderFormField;", ctx);
const { renderFormField } = ctx;

test("renderFormField: type=number emits a native-validated input with min/max/step", () => {
  const html = renderFormField({ name: "max_concurrency", label: "Max concurrency", type: "number", min: 0, value: 4 });
  assert.match(html, /<input type="number"/, "must be a type=number input");
  assert.match(html, /min="0"/, "must carry the min constraint");
  assert.match(html, /value="4"/);
});

test("renderFormField: type=number omits min/max/step when unset", () => {
  const html = renderFormField({ name: "x", label: "X", type: "number", value: 1 });
  assert.doesNotMatch(html, /min=/);
  assert.doesNotMatch(html, /max=/);
  assert.doesNotMatch(html, /step=/);
});

test("renderFormField: the default (no type) fallback stays a plain text input — the gap this fix closes for every OTHER field shape", () => {
  const html = renderFormField({ name: "name", label: "Name", value: "x" });
  assert.doesNotMatch(html, /type="number"/);
  assert.match(html, /<input name="name"/);
});

test("renderFormField: checkbox/textarea/select/password branches are unaffected by the new number branch", () => {
  assert.match(renderFormField({ name: "c", label: "C", type: "checkbox", value: true }), /type="checkbox"[\s\S]*checked/);
  assert.match(renderFormField({ name: "t", label: "T", type: "textarea", value: "hi" }), /<textarea name="t">hi<\/textarea>/);
  assert.match(renderFormField({ name: "s", label: "S", type: "select", options: ["a", "b"], value: "b" }), /<option value="b" selected>b<\/option>/);
  assert.match(renderFormField({ name: "p", label: "P", type: "password", value: "" }), /type="password"/);
});
