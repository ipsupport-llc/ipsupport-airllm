// Node-native test (node --test) for api()'s session-expiry handling: once
// signed in, a 401 from the control API means the session cookie expired and
// the console must fall back to the login form instead of rendering empty
// views. Same extraction approach as staleguard.test.mjs — app.js is a plain
// browser script, so the real api() source is evaluated in isolation with
// fetch/me/sessionExpired stubbed on the context.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const match = src.match(/async function api\([\s\S]*?\n}\n/);
assert.ok(match, "api function not found in app.js");

function harness({ status, me }) {
  const ctx = {
    me,
    expired: 0,
    sessionExpired() { ctx.expired++; },
    fetch: async () => ({ ok: status < 400, status, json: async () => ({}) }),
  };
  vm.createContext(ctx);
  vm.runInContext(match[0] + "\nthis.api = api;", ctx);
  return ctx;
}

test("api: 401 while signed in triggers sessionExpired and never resolves", async () => {
  const ctx = harness({ status: 401, me: { subject: "admin" } });
  let settled = false;
  ctx.api("GET", "/api/admin/aliases").then(() => { settled = true; }, () => { settled = true; });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(ctx.expired, 1);
  assert.equal(settled, false, "the calling view must not continue after the session is gone");
});

test("api: 401 while signed out (login form) is left to the caller", async () => {
  const ctx = harness({ status: 401, me: null });
  await ctx.api("POST", "/auth/login", { username: "x", password: "y" });
  assert.equal(ctx.expired, 0);
});

test("api: non-401 errors do not end the session", async () => {
  for (const status of [200, 400, 403, 500]) {
    const ctx = harness({ status, me: { subject: "admin" } });
    await ctx.api("GET", "/api/admin/aliases");
    assert.equal(ctx.expired, 0, `status ${status}`);
  }
});
