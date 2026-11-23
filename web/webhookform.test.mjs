// Node-native test (node --test) for webhookForm, the alert-webhook form
// description shared by create and edit. Same extraction technique as
// web/staleguard.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const m = src.match(/function webhookForm\(h\)[\s\S]*?\n}\n/);
assert.ok(m, "webhookForm not found in app.js");
const ctx = {};
vm.createContext(ctx);
vm.runInContext(m[0] + "\nthis.webhookForm = webhookForm;", ctx);
const { webhookForm } = ctx;
const field = (f, name) => f.fields.find((x) => x.name === name);

test("new webhook: POST, empty fields, dlp.incident preselected, enabled", () => {
  const f = webhookForm(undefined);
  assert.equal(f.method, "POST");
  assert.equal(f.path, "/api/admin/webhooks");
  assert.equal(field(f, "url").value, "");
  assert.equal(JSON.stringify(field(f, "events").value), '["dlp.incident"]');
  assert.equal(field(f, "enabled").value, true);
});

test("edit: PUT to the id, prefilled, secret blank and labeled keep", () => {
  const f = webhookForm({ id: "a/b", name: "sec", url: "https://h.test/x", events: ["dlp.alert_cleared", "custom.event"], enabled: false, has_secret: true });
  assert.equal(f.method, "PUT");
  assert.equal(f.path, "/api/admin/webhooks/a%2Fb");
  assert.equal(field(f, "name").value, "sec");
  assert.equal(field(f, "url").value, "https://h.test/x");
  assert.equal(field(f, "secret").value, "");
  assert.match(field(f, "secret").label, /leave empty to keep/);
  assert.equal(field(f, "enabled").value, false);
  const ev = field(f, "events");
  assert.equal(JSON.stringify(ev.value), '["dlp.alert_cleared","custom.event"]');
  assert.ok(ev.options.includes("custom.event"), "an unknown stored event must stay selectable");
  assert.ok(ev.options.includes("dlp.incident"));
});
