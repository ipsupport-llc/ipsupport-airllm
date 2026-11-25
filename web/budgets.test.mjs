// Node-native test (node --test) for the per-user budget and user group
// helpers: compact counts, zoned period times, percent/threshold classes,
// the dashboard bars, the users-list spend cell, budget-form parsing, the
// group report order and the Live page roll-up. Same extraction technique
// as web/staleguard.test.mjs.
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
const names = ["fmtUSD", "fmtCompact", "fmtZoned", "fmtPeriodRange", "budgetPct", "budgetLevel", "budgetBars",
  "monthSpend", "parseBudgets", "orderGroupSpend", "liveSummary"];
const ctx = {};
vm.createContext(ctx);
vm.runInContext(names.map(grab).join("\n") + `\nObject.assign(this, { ${names.join(", ")} });`, ctx);
// Values built in the vm realm fail deepEqual's prototype check; compare plain copies.
const plain = (v) => JSON.parse(JSON.stringify(v));

test("fmtCompact", () => {
  assert.equal(ctx.fmtCompact(0), "0");
  assert.equal(ctx.fmtCompact(950), "950");
  assert.equal(ctx.fmtCompact(12500), "12.5K");
  assert.equal(ctx.fmtCompact(999999), "1M");
  assert.equal(ctx.fmtCompact(1234567), "1.2M");
  assert.equal(ctx.fmtCompact(2000000), "2M");
  assert.equal(ctx.fmtCompact(3e9), "3B");
});

test("fmtZoned: wall time in the given zone, UTC fallback for a bad zone", () => {
  assert.equal(ctx.fmtZoned("2026-12-01T00:00:00Z", "UTC"), "2026-12-01 00:00 UTC");
  assert.equal(ctx.fmtZoned("2026-12-01T05:00:00Z", "America/New_York"), "2026-12-01 00:00 America/New_York");
  assert.equal(ctx.fmtZoned("2026-11-30T23:00:00Z", "Europe/Berlin"), "2026-12-01 00:00 Europe/Berlin");
  assert.equal(ctx.fmtZoned("2026-12-01T00:00:00Z", "Not/AZone"), "2026-12-01 00:00 UTC");
  assert.equal(ctx.fmtZoned("2026-12-01T00:00:00Z", ""), "2026-12-01 00:00 UTC");
  assert.equal(ctx.fmtZoned("", "UTC"), "—");
  assert.equal(ctx.fmtZoned("garbage", "UTC"), "—");
});

test("fmtPeriodRange names the zone once", () => {
  assert.equal(ctx.fmtPeriodRange("2026-11-01T00:00:00Z", "2026-12-01T00:00:00Z", "UTC"),
    "2026-11-01 00:00 – 2026-12-01 00:00 UTC");
});

test("budgetPct and budgetLevel: no cap, thresholds at 80 and 100, overshoot kept", () => {
  assert.equal(ctx.budgetPct(5, 0), null);
  assert.equal(ctx.budgetPct(5, undefined), null);
  assert.equal(ctx.budgetPct(25, 50), 50);
  assert.equal(ctx.budgetPct(55, 50), 110);
  assert.equal(ctx.budgetLevel(null), "");
  assert.equal(ctx.budgetLevel(79.9), "");
  assert.equal(ctx.budgetLevel(80), "warn");
  assert.equal(ctx.budgetLevel(99.99), "warn");
  assert.equal(ctx.budgetLevel(100), "over");
  assert.equal(ctx.budgetLevel(140), "over");
});

test("budgetBars: only capped period/unit pairs, in order; empty without caps", () => {
  const budget = { tz: "UTC", periods: {
    day: { start: "a", end: "2026-11-25T00:00:00Z", tokens_used: 10, tokens_limit: 0, cost_usd_used: 5, cost_usd_limit: 5 },
    week: { start: "b", end: "2026-11-30T00:00:00Z", tokens_used: 1600000, tokens_limit: 2000000, cost_usd_used: 9, cost_usd_limit: 0 },
    month: { start: "c", end: "2026-12-01T00:00:00Z", tokens_used: 0, tokens_limit: 0, cost_usd_used: 12.4, cost_usd_limit: 50 },
  } };
  const bars = plain(ctx.budgetBars(budget));
  assert.deepEqual(bars.map((b) => [b.period, b.unit, b.level]), [
    ["day", "cost_usd", "over"], ["week", "tokens", "warn"], ["month", "cost_usd", ""],
  ]);
  assert.equal(bars[0].end, "2026-11-25T00:00:00Z");
  assert.equal(bars[2].pct, 24.8);
  assert.equal(ctx.budgetBars({ tz: "UTC", periods: { day: { tokens_limit: 0, cost_usd_limit: 0 } } }).length, 0);
  assert.equal(ctx.budgetBars(undefined).length, 0);
});

test("monthSpend: cost vs cap, optional tokens line, worst level", () => {
  assert.deepEqual(plain(ctx.monthSpend({ spend: { month: { tokens: 10, cost_usd: 12.4 } }, budgets: { cost_usd: { month: 50 } } })),
    { cost: "$12.40 / $50.00", tokens: "", level: "" });
  assert.deepEqual(plain(ctx.monthSpend({ spend: { month: { tokens: 1900000, cost_usd: 12.4 } },
    budgets: { cost_usd: { month: 50 }, tokens: { month: 2000000 } } })),
    { cost: "$12.40 / $50.00", tokens: "1.9M / 2M tok", level: "warn" });
  assert.equal(ctx.monthSpend({ spend: { month: { cost_usd: 50 } }, budgets: { cost_usd: { month: 50 } } }).level, "over");
  // No caps, no spend block at all (an older backend): plain $0.
  assert.deepEqual(plain(ctx.monthSpend({})), { cost: "$0.00", tokens: "", level: "" });
});

test("parseBudgets: all six cells sent, empty is 0, bad values rejected", () => {
  assert.deepEqual(plain(ctx.parseBudgets({ "tokens.week": "2000000", "cost_usd.day": " 5 ", "cost_usd.month": "49.5" })), {
    tokens: { day: 0, week: 2000000, month: 0 },
    cost_usd: { day: 5, week: 0, month: 49.5 },
  });
  assert.deepEqual(plain(ctx.parseBudgets({})), { tokens: { day: 0, week: 0, month: 0 }, cost_usd: { day: 0, week: 0, month: 0 } });
  assert.throws(() => ctx.parseBudgets({ "cost_usd.week": "-1" }), /Cost \$ per week must be a non-negative number/);
  assert.throws(() => ctx.parseBudgets({ "tokens.day": "abc" }), /Tokens per day must be a non-negative number/);
  assert.throws(() => ctx.parseBudgets({ "tokens.month": "1.5" }), /Tokens per month must be a whole number/);
});

test("orderGroupSpend moves Ungrouped last and keeps the rest in API order", () => {
  const g = ctx.orderGroupSpend([
    { group_id: null, name: "Ungrouped", cost_usd: 99 },
    { group_id: "g2", name: "finance", cost_usd: 50 },
    { group_id: "g1", name: "developers", cost_usd: 10 },
  ]);
  assert.deepEqual(g.map((x) => x.name), ["finance", "developers", "Ungrouped"]);
  assert.equal(ctx.orderGroupSpend(undefined).length, 0);
});

test("liveSummary: per user and per group, ownerless keys counted, Ungrouped last", () => {
  const reqs = [
    { user: "alice", group: "developers", key_name: "laptop" },
    { user: "bob", group: "", key_name: "rag" },
    { user: "alice", group: "developers", key_name: "laptop" },
    { user: "", group: "", key_name: "ci-bot" },
    { user: "carol", group: "finance", key_name: "x" },
  ];
  const spend = { alice: { tokens: 1000, cost_usd: 1.5 }, bob: { tokens: 10, cost_usd: 0.25 }, carol: { tokens: 5, cost_usd: 2 } };
  const s = plain(ctx.liveSummary(reqs, spend));
  assert.deepEqual(s.users.map((u) => [u.user || "key:" + u.key_name, u.group, u.inflight, u.cost_usd]), [
    ["alice", "developers", 2, 1.5],
    ["bob", "", 1, 0.25],
    ["carol", "finance", 1, 2],
    ["key:ci-bot", "", 1, null],
  ]);
  assert.deepEqual(s.groups.map((g) => [g.group, g.users, g.inflight, g.tokens, g.cost_usd]), [
    ["developers", 1, 2, 1000, 1.5],
    ["finance", 1, 1, 5, 2],
    ["", 2, 2, 10, 0.25],
  ]);
  assert.equal(s.users.reduce((n, u) => n + u.inflight, 0), reqs.length);

  // Spend not loaded yet: everything unknown, counts still right.
  const n = plain(ctx.liveSummary(reqs, null));
  assert.ok(n.users.every((u) => u.tokens === null && u.cost_usd === null));
  assert.ok(n.groups.every((g) => g.tokens === null));
  assert.equal(ctx.liveSummary(null, null).users.length, 0);
});

test("renderFormField budgets: six cells, stored caps filled, 0 shown empty, label escaped", () => {
  const f = vm.createContext({});
  vm.runInContext(["esc", "renderFormField"].map(grab).join("\n") + "\nthis.renderFormField = renderFormField;", f);
  const html = f.renderFormField({ name: "budgets", type: "budgets", label: "Budget <tz>",
    value: { tokens: { week: 2000000, day: 0 }, cost_usd: { month: 50 } } });
  assert.equal((html.match(/data-budget="/g) || []).length, 6);
  assert.match(html, /data-budget="tokens\.week" value="2000000"/);
  assert.match(html, /data-budget="tokens\.day" value=""/);
  assert.match(html, /data-budget="cost_usd\.month" value="50"/);
  assert.match(html, /data-budget="tokens\.day"[^>]*step="1"|step="1"[^>]*data-budget="tokens\.day"/);
  assert.match(html, /Budget &lt;tz&gt;/);
});

test("liveSummary: group and user names that are Object.prototype keys", () => {
  const reqs = [
    { user: "constructor", group: "__proto__", key_name: "k1" },
    { user: "toString", group: "constructor", key_name: "k2" },
  ];
  const s = plain(ctx.liveSummary(reqs, { toString: { tokens: 5, cost_usd: 0.5 } }));
  assert.deepEqual(s.groups.map((g) => g.group).sort(), ["__proto__", "constructor"]);
  const byUser = Object.fromEntries(s.users.map((u) => [u.user, u]));
  assert.equal(byUser.constructor.tokens, null);
  assert.equal(byUser.toString.tokens, 5);
  assert.equal({}.inflight, undefined);
});
