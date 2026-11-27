"use strict";

// ---------- helpers ----------
const $ = (sel, root = document) => root.querySelector(sel);
const app = $("#app");

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// staleGuard protects one view/loader against out-of-order async
// responses: each call to start() marks a new "latest" request; a result
// is current() only if no later call has started since. A loader that can
// be re-triggered before its previous fetch resolves (navigating back to a
// view, clicking a filter/action again before the first reload finished)
// would otherwise let an earlier, now-stale response overwrite a later
// one's already-rendered, fresher content.
function staleGuard() {
  let gen = 0;
  return { start: () => ++gen, current: (token) => token === gen };
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let res;
  // A network failure (gateway restarting, connection reset) is reported
  // like any other failed call, so views show their error state instead of
  // an unhandled rejection leaving a half-rendered page.
  try { res = await fetch(path, opts); } catch (_) {
    return { ok: false, status: 0, data: { error: "Network error — the gateway did not answer" } };
  }
  let data = null;
  try { data = await res.json(); } catch (_) { /* no body */ }
  // On a signed-in console the control API only answers 401 when the session
  // is gone (cookie past its TTL, or the account was disabled). Without this
  // every view kept rendering empty until a manual reload revealed the login
  // form. While signed out (me === null) a 401 is the login form's own
  // "invalid credentials" and is left to the caller. The returned promise
  // never settles, so the calling view stops instead of writing into (or
  // toasting errors over) the shell it no longer owns.
  if (res.status === 401 && me) { sessionExpired(); return new Promise(() => {}); }
  return { ok: res.ok, status: res.status, data };
}

// sessionExpired drops back to the login form, keeping location.hash so a
// fresh sign-in (init -> renderShell -> route) reopens the same page.
function sessionExpired() {
  stopLive();
  stopGroupSpend();
  me = null;
  window.onhashchange = null;
  renderLogin();
  toast("Session expired — please sign in again", "err");
}

function toast(msg, kind = "ok") {
  const t = document.createElement("div");
  t.className = "toast " + kind;
  t.textContent = msg;
  $("#toasts").appendChild(t);
  setTimeout(() => t.remove(), 3500);
}

// fmtUSD groups thousands and shows cents for amounts of a dollar or more;
// smaller amounts keep up to 4 decimals so a $0.0012 request is not "$0.00".
function fmtUSD(n) {
  const v = Number(n) || 0;
  return "$" + v.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: Math.abs(v) >= 1 ? 2 : 4 });
}
function fmtNum(n) { return Number(n || 0).toLocaleString("en-US"); }
function fmtTime(s) { return s ? new Date(s).toLocaleString() : "—"; }
// sparkline renders an inline SVG polyline from numeric values. No deps.
function sparkline(values, opts) {
  const o = Object.assign({ w: 220, h: 40, stroke: "var(--accent)" }, opts || {});
  const vals = (values && values.length) ? values : [0];
  const max = Math.max(...vals, 1), min = Math.min(...vals, 0);
  const span = (max - min) || 1;
  const step = vals.length > 1 ? o.w / (vals.length - 1) : o.w;
  const pts = vals.map((v, i) => `${(i * step).toFixed(1)},${(o.h - ((v - min) / span) * o.h).toFixed(1)}`).join(" ");
  return `<svg viewBox="0 0 ${o.w} ${o.h}" width="100%" height="${o.h}" preserveAspectRatio="none" style="display:block">
    <polyline fill="none" stroke="${esc(o.stroke)}" stroke-width="2" points="${pts}" /></svg>`;
}
// Render a role limits object ({tokens:{24h:N}, usd:{7d:N}}) as readable text.
function fmtLimits(lim) {
  const parts = [];
  for (const [dim, windows] of Object.entries(lim || {})) {
    for (const [win, val] of Object.entries(windows || {})) {
      parts.push(dim === "cost_usd" ? `$${fmtNum(val)}/${esc(win)}` : `${fmtNum(val)} ${esc(dim)}/${esc(win)}`);
    }
  }
  return parts.length ? parts.join(", ") : "—";
}

// shortVersion shortens a build stamp for the sidebar: "main-<40-hex sha>"
// becomes "main-<7-hex>", a plain release "0.1.86" becomes "v0.1.86".
// Top-level so web/consolehelpers.test.mjs can extract it.
function shortVersion(v) {
  const s = String(v || "dev");
  const m = s.match(/^(.*?)-?([0-9a-f]{12,40})$/);
  if (m) return (m[1] ? m[1] + "-" : "") + m[2].slice(0, 7);
  return /^\d+\.\d+/.test(s) ? "v" + s : s;
}

// apiError is the message to show for a failed api() result.
function apiError(r) {
  return (r && r.data && r.data.error) || (r && r.status ? `HTTP ${r.status}` : "Request failed");
}

// loadFailed renders an error panel with a Retry button into el when any of
// the results failed, and reports whether it did. Views call it before
// rendering so a failed GET never shows up as "nothing here" or $0 — and,
// for editors, never becomes the starting point of a save.
function loadFailed(el, results, retry) {
  const bad = results.find((r) => !r || !r.ok);
  if (!bad || !el) return false;
  el.innerHTML = `<div class="panel load-error"><div class="panel-head"><h2>Could not load</h2></div>
    <div class="load-error-body"><p class="err-text">${esc(apiError(bad))}</p>
    <button class="btn sm" type="button" data-retry>Retry</button></div></div>`;
  el.querySelector("[data-retry]").addEventListener("click", retry);
  return true;
}

let me = null;
let authMode = { mode: "local" };

// isAuditor returns true when the signed-in user holds the auditor or admin
// role. Used to gate the Captures nav link and route.
function isAuditor() { return me && (me.is_admin || me.is_auditor); }

// ---------- bootstrap ----------
async function init() {
  const [r, modeR] = await Promise.all([api("GET", "/api/me"), api("GET", "/api/auth/mode")]);
  authMode = modeR.data || { mode: "local" };
  if (r.status === 401) { renderLogin(); return; }
  if (!r.ok) { app.innerHTML = `<div class="login-wrap"><div class="login-card"><h1>AirLLM</h1><p>Backend unavailable.</p></div></div>`; return; }
  me = r.data;
  renderShell();
}

// ---------- login ----------
function renderLogin() {
  if (authMode.mode === "oidc") {
    app.innerHTML = `
    <div class="login-wrap">
      <div class="login-card">
        <h1>Air<span>LLM</span></h1>
        <p>Sign in to the gateway console.</p>
        <a class="btn" href="${esc(authMode.sso_url || "/auth/sso")}" style="width:100%;text-align:center;display:block">Sign in with SSO</a>
        <div class="copyright">© 2026 <a href="https://ipsupport.us" target="_blank" rel="noopener">IPSupport LLC</a></div>
      </div>
    </div>`;
    return;
  }
  app.innerHTML = `
    <div class="login-wrap">
      <div class="login-card">
        <h1>Air<span>LLM</span></h1>
        <p>Sign in to the gateway console.</p>
        <form id="login-form">
          <label class="field"><span class="lab">Username</span>
            <input name="username" autocomplete="username" autofocus /></label>
          <label class="field"><span class="lab">Password</span>
            <input name="password" type="password" autocomplete="current-password" /></label>
          <button class="btn" style="width:100%" type="submit">Sign in</button>
          <p class="err-text" id="login-err"></p>
        </form>
        <div class="copyright">© 2026 <a href="https://ipsupport.us" target="_blank" rel="noopener">IPSupport LLC</a></div>
      </div>
    </div>`;
  $("#login-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    const r = await api("POST", "/auth/login", {
      username: f.username.value, password: f.password.value,
    });
    if (r.ok) { init(); }
    else { $("#login-err").textContent = (r.data && r.data.error) || "Login failed"; }
  });
}

// ---------- shell ----------
const NAV = [
  { href: "#/", label: "Dashboard" },
  { href: "#/keys", label: "API Keys" },
  { href: "#/usage", label: "Usage" },
];
const ADMIN_TABS = ["users", "groups", "keys", "usage", "roles", "aliases", "providers", "pricing", "dlp", "audit"];

function renderShell() {
  // Live is an org-wide view, so it sits with the other admin-only views.
  const adminLink = me.is_admin ? `<div class="sect">Admin</div><a href="#/live" data-nav>Live</a><a href="#/admin/users" data-nav>Admin console</a>` : "";
  const capturesLink = isAuditor() ? `<a href="#/captures" data-nav>Captures</a><a href="#/review" data-nav>Review</a>` : "";
  app.innerHTML = `
    <div class="shell">
      <aside class="sidebar">
        <div class="brand">Air<span>LLM</span></div>
        <nav class="nav">
          ${NAV.map((n) => `<a href="${n.href}" data-nav>${n.label}</a>`).join("")}
          ${capturesLink}
          ${adminLink}
        </nav>
        <div class="sidebar-foot">
          <div class="who-block">
            <div class="who">${esc(me.subject)}</div>
            <div class="roles">${esc((me.roles || []).join(", ")) || "no roles"}</div>
          </div>
          <div class="foot-actions">
            ${authMode.mode !== "oidc" ? `<button class="btn ghost sm" id="change-pw">Change password</button>` : ""}
            <button class="btn ghost sm" id="logout">Sign out</button>
          </div>
          <div class="ver" title="AirLLM ${esc(me.version || "dev")}">AirLLM ${esc(shortVersion(me.version))}</div>
          <div class="copyright">© 2026 <a href="https://ipsupport.us" target="_blank" rel="noopener">IPSupport LLC</a></div>
        </div>
      </aside>
      <main class="main"><div id="view"></div></main>
    </div>`;
  const cpBtn = $("#change-pw");
  if (cpBtn) cpBtn.addEventListener("click", changePassword);
  $("#logout").addEventListener("click", async () => { stopLive(); stopGroupSpend(); await api("POST", "/auth/logout"); me = null; renderLogin(); });
  window.onhashchange = route;
  route();
}

function changePassword() {
  modalForm("Change password", [
    { name: "current", label: "Current password", type: "password", value: "" },
    { name: "new", label: "New password", type: "password", value: "" },
  ], async (v) => {
    if (!v.current || !v["new"]) { toast("Both fields are required", "err"); return false; }
    const x = await api("POST", "/api/me/password", { current: v.current, new: v["new"] });
    if (x.ok) { toast("Password changed"); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

function setActiveNav() {
  const hash = location.hash || "#/";
  document.querySelectorAll("[data-nav]").forEach((a) => {
    const href = a.getAttribute("href");
    const on = href === hash || (href === "#/admin/users" && hash.startsWith("#/admin")) || (href === "#/review" && hash === "#/review");
    a.classList.toggle("active", on);
  });
}

function route() {
  stopLive();
  stopGroupSpend();
  setActiveNav();
  const view = $("#view");
  const hash = location.hash || "#/";
  if (hash.startsWith("#/admin")) {
    if (!me.is_admin) { view.innerHTML = `<p class="err-text">Admin role required.</p>`; return; }
    const tab = hash.split("/")[2] || "users";
    viewAdmin(view, tab);
  } else if (hash === "#/captures") {
    if (!isAuditor()) { view.innerHTML = `<p class="err-text">Auditor role required.</p>`; return; }
    viewCaptures(view);
  } else if (hash === "#/review") {
    if (!isAuditor()) { view.innerHTML = `<p class="err-text">Auditor role required.</p>`; return; }
    viewReview(view);
  } else if (hash === "#/live") {
    if (!me.is_admin) { view.innerHTML = `<p class="err-text">Admin role required.</p>`; return; }
    viewLive(view);
  } else if (hash === "#/keys") viewKeys(view);
  else if (hash === "#/usage") viewUsage(view);
  else viewDashboard(view);
}

// ---------- dashboard ----------
const dashGuard = staleGuard();
async function viewDashboard(view) {
  const token = dashGuard.start();
  view.innerHTML = `<h1 class="page-title">Dashboard</h1><div id="dash"></div>`;
  const seriesPath = me.is_admin ? "/api/admin/usage/series" : "/api/usage/series";
  // /api/me is fetched again (not the copy from sign-in) so the budget
  // bars show current spend.
  const [u, k, sr, m] = await Promise.all([api("GET", "/api/usage"), api("GET", "/api/keys"), api("GET", seriesPath), api("GET", "/api/me")]);
  if (!dashGuard.current(token)) return;
  if (loadFailed($("#dash"), [u, k, sr, m], () => viewDashboard(view))) return;
  const usage = u.data || {};
  const keys = (k.data && k.data.keys) || [];
  const active = keys.filter((x) => x.status === "active").length;
  const series = (sr.data && sr.data.series) || [];
  const spark = (label, key, fmt) => {
    const vals = series.map((p) => Number(p[key]) || 0);
    const last = vals.length ? vals[vals.length - 1] : 0;
    return `<div class="card"><div class="sub">${esc(label)} · 24h</div>
      <div class="value">${fmt(last)}</div>${sparkline(vals)}</div>`;
  };
  // The cards above are always the signed-in user's own keys; an admin's
  // sparklines are organization-wide, so each block says whose it is.
  const sparksHtml = series.length
    ? `<h2 class="section-label">${me.is_admin ? "Organization · all keys" : "Your keys"} · hourly</h2>
      <div class="cards">
        ${spark("Tokens/hr", "tokens", fmtNum)}
        ${spark("Cost/hr", "cost_usd", fmtUSD)}
        ${spark("Requests/hr", "requests", fmtNum)}
        ${spark("p95 latency", "p95_ms", (v) => fmtNum(v) + " ms")}
      </div>`
    : `<div class="empty" style="margin-top:1rem">No usage history yet.</div>`;
  $("#dash").innerHTML = `
    ${budgetSection((m.data || {}).budget)}
    <h2 class="section-label">Your usage</h2>
    ${usageCards(usage)}
    ${sparksHtml}
    <div class="panel">
      <div class="panel-head"><h2>Your keys</h2><a class="btn sm" href="#/keys">Manage</a></div>
      <div class="empty">${active} active key${active === 1 ? "" : "s"} of ${keys.length} total.</div>
    </div>
    ${connectPanel()}`;
}

// budgetSection renders the signed-in user's budget bars, one card per
// capped period and unit, or nothing when no cap is set.
function budgetSection(budget) {
  const bars = budgetBars(budget);
  if (!bars.length) return "";
  const tz = (budget && budget.tz) || "UTC";
  const period = { day: "Today", week: "This week", month: "This month" };
  const card = (b) => {
    const cost = b.unit === "cost_usd";
    const used = cost ? fmtUSD(b.used) : fmtCompact(b.used);
    const limit = cost ? fmtUSD(b.limit) : fmtCompact(b.limit) + " tokens";
    const full = cost ? "" : ` title="${esc(fmtNum(b.used))} of ${esc(fmtNum(b.limit))} tokens"`;
    const state = b.level === "over" ? `<span class="badge revoked">limit reached</span>` : `${Math.floor(b.pct)}% used`;
    return `<div class="card budget-card ${b.level}">
      <div class="label">${period[b.period]} · ${cost ? "cost" : "tokens"}</div>
      <div class="value"${full}>${used} <span class="of">of ${limit}</span></div>
      <div class="meter" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${Math.min(100, Math.round(b.pct))}"><span style="width:${Math.min(100, b.pct).toFixed(1)}%"></span></div>
      <div class="sub">${state} · resets ${esc(fmtZoned(b.end, tz))}</div>
    </div>`;
  };
  return `<h2 class="section-label">Your budget</h2><div class="cards">${bars.map(card).join("")}</div>`;
}

function usageCards(u) {
  const w = (k) => (u[k] || { tokens: 0, cost_usd: 0 });
  const card = (label, win) => `
    <div class="card">
      <div class="label">${label}</div>
      <div class="value">${fmtNum(win.tokens)}</div>
      <div class="sub">tokens · ${fmtUSD(win.cost_usd)}</div>
    </div>`;
  return `<div class="cards">${card("Last 5h", w("5h"))}${card("Last 24h", w("24h"))}${card("Last 7d", w("7d"))}</div>`;
}

// connectPanel shows the gateway endpoints (derived from this origin) so a
// user knows exactly where to point their client.
function connectPanel() {
  const base = location.origin;
  const row = (k, v) => `<tr><td>${esc(k)}</td><td class="mono">${esc(v)}</td></tr>`;
  return `<div class="panel">
    <div class="panel-head"><h2>Connect</h2></div>
    <div style="padding:.6rem 0">
      <table>
        ${row("OpenAI base URL", base + "/v1")}
        ${row("Chat completions", "POST " + base + "/v1/chat/completions")}
        ${row("Models", "GET " + base + "/v1/models")}
        ${row("Anthropic messages", "POST " + base + "/v1/messages")}
        ${row("Auth header", "Authorization: Bearer <key>   (or x-api-key)")}
      </table>
    </div>
    <div style="padding:0 1.1rem 1rem">
      <div class="token-box" style="border-color:var(--rule)">curl ${esc(base)}/v1/chat/completions -H "Authorization: Bearer &lt;key&gt;" -d '{"model":"mock-gpt","messages":[{"role":"user","content":"hi"}]}'</div>
    </div>
  </div>`;
}

// ---------- usage ----------
const usageGuard = staleGuard();
async function viewUsage(view) {
  const token = usageGuard.start();
  view.innerHTML = `<h1 class="page-title">Usage</h1><div id="u"></div>`;
  const [u, b] = await Promise.all([
    api("GET", "/api/usage"),
    api("GET", "/api/usage/breakdown?hours=24"),
  ]);
  if (!usageGuard.current(token)) return;
  if (loadFailed($("#u"), [u, b], () => viewUsage(view))) return;
  $("#u").innerHTML = usageCards(u.data || {}) +
    `<p class="card-sub" style="color:var(--muted)">Rolling windows, enforced per API key. Limits are configured by your role policy.</p>` +
    breakdownTables(b.data || {});
}

// ---------- captures (auditor view) ----------
async function viewCaptures(view) {
  view.innerHTML = `
    <h1 class="page-title">Captures</h1>
    <div class="panel">
      <div class="panel-head"><h2>Filter</h2></div>
      <div style="padding:.8rem 1.1rem">
        <div class="row" style="flex-wrap:wrap;gap:.5rem">
          <label class="field" style="flex:0 1 180px;margin:0">
            <span class="lab">Review status</span>
            <select id="cap-status">
              <option value="">all</option>
              <option value="unreviewed">unreviewed</option>
              <option value="confirmed">confirmed</option>
              <option value="false_positive">false_positive</option>
              <option value="false_negative">false_negative</option>
            </select>
          </label>
          <label class="field" style="flex:0 1 200px;margin:0">
            <span class="lab">From (UTC)</span>
            <input type="datetime-local" id="cap-from" />
          </label>
          <label class="field" style="flex:0 1 200px;margin:0">
            <span class="lab">To (UTC)</span>
            <input type="datetime-local" id="cap-to" />
          </label>
          <label class="field" style="flex:0 1 100px;margin:0">
            <span class="lab">Limit</span>
            <input type="number" id="cap-limit" value="50" min="1" max="500" />
          </label>
          <div style="display:flex;align-items:flex-end">
            <button class="btn" id="cap-search">Search</button>
          </div>
        </div>
      </div>
    </div>
    <div id="cap-results"></div>
    <div id="cap-drawer"></div>`;
  $("#cap-search").addEventListener("click", loadCaptures);
  await loadCaptures();
}

const capturesGuard = staleGuard();
async function loadCaptures() {
  const token = capturesGuard.start();
  const status = ($("#cap-status") && $("#cap-status").value) || "";
  const limit = ($("#cap-limit") && $("#cap-limit").value) || "50";
  const fromEl = $("#cap-from");
  const toEl = $("#cap-to");

  const params = new URLSearchParams();
  if (status) params.set("review_status", status);
  if (limit) params.set("limit", limit);
  if (fromEl && fromEl.value) params.set("from", utcInputToISO(fromEl.value));
  if (toEl && toEl.value) params.set("to", utcInputToISO(toEl.value));

  const r = await api("GET", "/api/audit/captures?" + params.toString());
  if (!capturesGuard.current(token)) return;
  const el = $("#cap-results");
  if (!el) return;
  if (loadFailed(el, [r], loadCaptures)) return;
  const captures = (r.data && r.data.captures) || [];

  const labelBadges = (findings) => {
    if (!findings || !findings.length) return `<span class="badge neutral">clean</span>`;
    return findings.map((f) => `<span class="badge revoked">${esc(f.label || "")}</span>`).join(" ");
  };

  el.innerHTML = panelTable(
    `${captures.length} capture${captures.length === 1 ? "" : "s"}`,
    ["Time", "Ingress", "Alias", "Provider", "Status", "Redacted", "DLP labels", "Review"],
    captures.map((c) => `<tr style="cursor:pointer" data-cid="${esc(c.id)}">
      <td>${fmtTime(c.ts)}</td>
      <td class="mono">${esc(c.ingress_protocol)}</td>
      <td class="mono">${esc(c.alias) || "—"}</td>
      <td>${esc(c.provider_name) || "—"}</td>
      <td>${statusBadge(c.status)}</td>
      <td>${c.redacted ? `<span class="badge neutral">yes</span>` : "no"}</td>
      <td>${labelBadges(c.detected)}</td>
      <td><span class="badge neutral">${esc(c.review_status || "unreviewed")}</span></td>
    </tr>`));

  el.querySelectorAll("[data-cid]").forEach((row) =>
    row.addEventListener("click", () => openCaptureDrawer(row.getAttribute("data-cid"))));
}

// utcInputToISO reads a datetime-local value ("2026-11-22T10:00") as UTC —
// the filter is labeled UTC, while new Date(value) would take it as local.
function utcInputToISO(v) {
  const d = new Date(v + (v.length === 16 ? ":00Z" : "Z"));
  return isNaN(d) ? "" : d.toISOString();
}

// statusBadge shows an HTTP status; 0/missing (no response recorded) is
// neutral rather than a green success.
function statusBadge(st) {
  const n = Number(st) || 0;
  if (!n) return `<span class="badge neutral">—</span>`;
  return `<span class="badge ${n < 300 ? "active" : "revoked"}">${n}</span>`;
}

async function openCaptureDrawer(id) {
  const drawer = $("#cap-drawer");
  if (!drawer) return;
  drawer.innerHTML = `<div class="panel" style="margin-top:1rem">
    <div class="panel-head"><h2>Loading transcript…</h2><button class="btn ghost sm" id="cap-close">Close</button></div>
    <div style="padding:1rem 1.1rem"><div class="empty">Fetching…</div></div>
  </div>`;
  $("#cap-close").addEventListener("click", () => { drawer.innerHTML = ""; });

  const r = await api("GET", `/api/audit/captures/${encodeURIComponent(id)}`);
  if (!r.ok) {
    drawer.innerHTML = `<div class="panel" style="margin-top:1rem">
      <div class="panel-head"><h2>Error</h2><button class="btn ghost sm" id="cap-close2">Close</button></div>
      <div style="padding:1rem 1.1rem"><p class="err-text">${esc((r.data && r.data.error) || "Failed to load capture")}</p></div>
    </div>`;
    const c2 = $("#cap-close2");
    if (c2) c2.addEventListener("click", () => { drawer.innerHTML = ""; });
    return;
  }

  const cap = (r.data && r.data.capture) || {};
  const body = (r.data && r.data.body) || "";

  let bodyHtml = "";
  if (body) {
    try {
      const parsed = JSON.parse(body);
      const msgs = parsed.messages || [];
      const resp = parsed.response || "";
      const lines = msgs.map((m) =>
        `<div class="cap-msg cap-${esc(m.role || "")}" style="margin:.5rem 0;padding:.5rem .8rem;border-radius:6px;background:var(--bg-soft,#1e1e2e);border-left:3px solid ${m.role === "user" ? "var(--accent,#7c3aed)" : "var(--rule,#333)"}">
          <div class="mono" style="font-size:.75rem;opacity:.6;margin-bottom:.25rem">${esc(m.role || "")}</div>
          <div>${esc(m.content || "")}</div>
        </div>`);
      if (resp) {
        lines.push(`<div class="cap-msg cap-assistant" style="margin:.5rem 0;padding:.5rem .8rem;border-radius:6px;background:var(--bg-soft,#1e1e2e);border-left:3px solid var(--rule,#333)">
          <div class="mono" style="font-size:.75rem;opacity:.6;margin-bottom:.25rem">assistant</div>
          <div>${esc(resp)}</div>
        </div>`);
      }
      bodyHtml = lines.join("") || `<div class="empty">No messages.</div>`;
    } catch (_) {
      bodyHtml = `<pre style="white-space:pre-wrap;font-size:.82rem;overflow-x:auto">${esc(body)}</pre>`;
    }
  } else {
    bodyHtml = `<div class="empty">Body unavailable (blob missing or not configured).</div>`;
  }

  const meta = [
    ["ID", cap.id || id],
    ["Time", fmtTime(cap.ts)],
    ["Ingress", cap.ingress_protocol || "—"],
    ["Model alias", cap.alias || "—"],
    ["Provider", cap.provider_name || "—"],
    ["Upstream model", cap.upstream_model || "—"],
    ["Status", String(cap.status || "—")],
    ["Tokens in", String(cap.prompt_tokens || 0)],
    ["Tokens out", String(cap.completion_tokens || 0)],
    ["Cost", fmtUSD(cap.cost_usd)],
    ["Redacted", cap.redacted ? "yes" : "no"],
    ["Review status", cap.review_status || "unreviewed"],
  ];

  drawer.innerHTML = `<div class="panel" style="margin-top:1rem">
    <div class="panel-head"><h2>Capture ${esc(id)}</h2>
      <button class="btn ghost sm" id="cap-close3">Close</button></div>
    <div style="padding:0 1.1rem 1rem">
      <table style="margin-bottom:1rem">
        ${meta.map(([k, v]) => `<tr><td style="opacity:.6;padding-right:1rem">${esc(k)}</td><td class="mono">${esc(v)}</td></tr>`).join("")}
      </table>
      <div class="lab" style="margin-bottom:.4rem">Transcript</div>
      ${bodyHtml}
    </div>
  </div>`;
  $("#cap-close3").addEventListener("click", () => { drawer.innerHTML = ""; });
  drawer.scrollIntoView({ behavior: "smooth" });
}

// ---------- review (auditor/admin) ----------
async function viewReview(view) {
  view.innerHTML = `
    <h1 class="page-title">Review queue</h1>
    <div id="rev-table"></div>
    <div id="rev-modal"></div>`;
  await loadReviewQueue();
}

async function loadReviewQueue() {
  const el = $("#rev-table");
  if (!el) return;
  const r = await api("GET", "/api/audit/review");
  if (loadFailed(el, [r], loadReviewQueue)) return;
  const captures = (r.data && r.data.captures) || [];

  const labelBadges = (findings) => {
    if (!findings || !findings.length) return `<span class="badge neutral">clean</span>`;
    return findings.map((f) => `<span class="badge revoked">${esc(f.label || "")}</span>`).join(" ");
  };

  el.innerHTML = panelTable(
    `${captures.length} pending`,
    ["Time", "Ingress", "Alias", "DLP labels", "Status", "Secondpass"],
    captures.map((c) => `<tr style="cursor:pointer" data-rid="${esc(c.id)}">
      <td>${fmtTime(c.ts)}</td>
      <td class="mono">${esc(c.ingress_protocol)}</td>
      <td class="mono">${esc(c.alias) || "—"}</td>
      <td>${labelBadges(c.detected)}</td>
      <td><span class="badge neutral">${esc(c.review_status || "unreviewed")}</span></td>
      <td><span class="badge neutral">${esc(c.secondpass_status || "—")}</span></td>
    </tr>`));

  el.querySelectorAll("[data-rid]").forEach((row) =>
    row.addEventListener("click", () => openReviewModal(row.getAttribute("data-rid"))));
}

async function openReviewModal(id) {
  const modal = $("#rev-modal");
  if (!modal) return;
  modal.innerHTML = `<div class="panel" style="margin-top:1rem">
    <div class="panel-head"><h2>Loading…</h2><button class="btn ghost sm" id="rev-close">Close</button></div>
    <div style="padding:1rem 1.1rem"><div class="empty">Fetching transcript…</div></div>
  </div>`;
  $("#rev-close").addEventListener("click", () => { modal.innerHTML = ""; });

  const r = await api("GET", `/api/audit/captures/${encodeURIComponent(id)}`);
  if (!r.ok) {
    modal.innerHTML = `<div class="panel" style="margin-top:1rem">
      <div class="panel-head"><h2>Error</h2><button class="btn ghost sm" id="rev-close2">Close</button></div>
      <div style="padding:1rem 1.1rem"><p class="err-text">${esc((r.data && r.data.error) || "Failed to load")}</p></div>
    </div>`;
    const c2 = $("#rev-close2"); if (c2) c2.addEventListener("click", () => { modal.innerHTML = ""; });
    return;
  }

  const cap = (r.data && r.data.capture) || {};
  const body = (r.data && r.data.body) || "";
  const detected = cap.detected || [];
  // The review endpoint replaces gold_labels wholesale, so every save sends
  // the reviewer labels already stored plus any new one.
  const gold = cap.gold_labels || [];
  let msgs = [];

  // Build transcript HTML.
  let bodyHtml = "";
  if (body) {
    try {
      const parsed = JSON.parse(body);
      msgs = parsed.messages || [];
      const resp = parsed.response || "";
      const lines = msgs.map((m) =>
        `<div style="margin:.4rem 0;padding:.4rem .7rem;border-radius:6px;background:var(--bg-soft,#1e1e2e);border-left:3px solid ${m.role === "user" ? "var(--accent,#7c3aed)" : "var(--rule,#333)"}">
          <div class="mono" style="font-size:.72rem;opacity:.6">${esc(m.role || "")}</div>
          <div>${esc(m.content || "")}</div>
        </div>`);
      if (resp) lines.push(`<div style="margin:.4rem 0;padding:.4rem .7rem;border-radius:6px;background:var(--bg-soft,#1e1e2e);border-left:3px solid var(--rule,#333)">
        <div class="mono" style="font-size:.72rem;opacity:.6">assistant</div>
        <div>${esc(resp)}</div>
      </div>`);
      bodyHtml = lines.join("") || `<div class="empty">No messages.</div>`;
    } catch (_) {
      bodyHtml = `<pre style="white-space:pre-wrap;font-size:.82rem">${esc(body)}</pre>`;
    }
  } else {
    bodyHtml = `<div class="empty">Body unavailable.</div>`;
  }

  // Detected spans summary. A tool-call finding has no span in content:
  // it is listed with the call it was found in and that call's arguments.
  const contentSpans = detected.filter((f) => !f.tool_call);
  const toolNotes = detected.filter((f) => f.tool_call).map((f) => {
    const n = toolCallFindingNote(f, msgs);
    return `<div style="margin:.2rem 0"><span class="badge revoked">${esc(f.label)}</span>
      in ${esc(n.where)}${n.excerpt ? `: <span class="mono" style="font-size:.8rem">${esc(n.excerpt)}</span>` : ""}</div>`;
  });
  const spansHtml = detected.length
    ? `<div style="margin-bottom:.5rem">${contentSpans.map((f) =>
        `<span class="badge revoked" title="start:${f.start} end:${f.end}">${esc(f.label)}</span>`).join(" ")}${toolNotes.join("")}</div>`
    : `<div class="empty" style="margin-bottom:.5rem">No detected spans.</div>`;

  modal.innerHTML = `<div class="panel" style="margin-top:1rem">
    <div class="panel-head"><h2>Review: ${esc(id)}</h2>
      <button class="btn ghost sm" id="rev-close3">Close</button></div>
    <div style="padding:0 1.1rem 1rem">
      <div class="lab" style="margin-bottom:.3rem">Detected spans</div>
      ${spansHtml}
      <div class="lab" style="margin-bottom:.3rem">Transcript</div>
      <div style="max-height:320px;overflow-y:auto;margin-bottom:1rem">${bodyHtml}</div>
      <div class="lab" style="margin-bottom:.4rem">Set review status</div>
      <div class="row" style="gap:.4rem;margin-bottom:1rem">
        <button class="btn sm" data-rev-status="confirmed">Confirmed</button>
        <button class="btn sm" data-rev-status="false_positive">False positive</button>
        <button class="btn sm" data-rev-status="false_negative">False negative</button>
        <button class="btn ghost sm" data-rev-status="unreviewed">Reset</button>
      </div>
      <div class="lab" style="margin-bottom:.3rem">Add missed span${gold.length ? ` <span class="badge neutral">${gold.length} stored</span>` : ""}</div>
      <div class="row" style="flex-wrap:wrap;gap:.4rem;align-items:flex-end;margin-bottom:.5rem">
        <label class="field" style="flex:0 1 160px;margin:0">
          <span class="lab">Label</span>
          <input id="rev-label" placeholder="e.g. openai_key" />
        </label>
        <label class="field" style="flex:1 1 220px;margin:0">
          <span class="lab">Missed text (paste — fills start/end)</span>
          <input id="rev-text" class="mono" placeholder="the secret as it appears in the transcript" />
        </label>
        <label class="field" style="flex:0 1 90px;margin:0">
          <span class="lab">Start</span>
          <input id="rev-start" type="number" min="0" value="0" />
        </label>
        <label class="field" style="flex:0 1 90px;margin:0">
          <span class="lab">End</span>
          <input id="rev-end" type="number" min="0" value="0" />
        </label>
        <button class="btn sm" id="rev-submit-fn">Submit as false_negative</button>
      </div>
      <p class="err-text" id="rev-err"></p>
    </div>
  </div>`;

  $("#rev-close3").addEventListener("click", () => { modal.innerHTML = ""; });
  modal.scrollIntoView({ behavior: "smooth" });

  $("#rev-text").addEventListener("input", () => {
    const t = $("#rev-text").value;
    const sp = t ? findSpan(msgs, t) : null;
    $("#rev-err").textContent = t && !sp ? "Text not found in any message." : "";
    if (sp) { $("#rev-start").value = sp.start; $("#rev-end").value = sp.end; }
  });

  // Status buttons (no extra span; stored gold labels are kept).
  modal.querySelectorAll("[data-rev-status]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const status = btn.getAttribute("data-rev-status");
      const res = await api("POST", `/api/audit/captures/${encodeURIComponent(id)}/review`,
        { review_status: status, labels: gold });
      if (res.ok) {
        toast(`Marked ${status}`);
        modal.innerHTML = "";
        loadReviewQueue();
      } else {
        toast((res.data && res.data.error) || "Failed", "err");
      }
    });
  });

  // False-negative submit (with a missed span).
  $("#rev-submit-fn").addEventListener("click", async () => {
    const label = ($("#rev-label").value || "").trim();
    const start = parseInt($("#rev-start").value || "0", 10);
    const end = parseInt($("#rev-end").value || "0", 10);
    const errEl = $("#rev-err");
    if (!label) { errEl.textContent = "Label is required."; return; }
    if (end <= start) { errEl.textContent = "End must be greater than start."; return; }
    errEl.textContent = "";
    const res = await api("POST", `/api/audit/captures/${encodeURIComponent(id)}/review`,
      { review_status: "false_negative", labels: [...gold, { label, start, end }] });
    if (res.ok) {
      toast("Submitted as false_negative");
      modal.innerHTML = "";
      loadReviewQueue();
    } else {
      errEl.textContent = (res.data && res.data.error) || "Failed";
    }
  });
}

// findSpan locates snippet in the first message whose content contains it
// and returns its UTF-8 byte offsets in that message — the unit dlp.Finding
// and the dataset export use. null when no message contains it.
function findSpan(messages, snippet) {
  const enc = new TextEncoder();
  for (const m of messages || []) {
    const c = typeof (m && m.content) === "string" ? m.content : "";
    const i = c.indexOf(snippet);
    if (i < 0) continue;
    const start = enc.encode(c.slice(0, i)).length;
    return { start, end: start + enc.encode(snippet).length };
  }
  return null;
}

// toolCallFindingNote describes a DLP finding located in a tool call's
// arguments (f.tool_call = {message, index, id}) rather than in message
// content: such a finding has no offsets, so it names the call and quotes
// the start of its arguments from the stored transcript (masked when the
// capture is redacted). where falls back to the call id or index when the
// transcript lacks the call.
function toolCallFindingNote(f, messages) {
  const ref = (f && f.tool_call) || {};
  const msg = (messages || [])[ref.message];
  const call = msg && Array.isArray(msg.tool_calls) ? msg.tool_calls[ref.index] : null;
  const fn = (call && call.function) || {};
  const name = fn.name || ref.id || `#${(ref.index || 0) + 1}`;
  let excerpt = typeof fn.arguments === "string" ? fn.arguments : "";
  if (excerpt.length > 160) excerpt = excerpt.slice(0, 160) + "…";
  return { where: `tool call ${name} (message ${(ref.message || 0) + 1})`, excerpt };
}

// ---------- budgets & groups: pure helpers ----------
// Per-user budgets cap tokens and cost per calendar day / ISO week / month
// in the gateway's LIMITS_TZ; groups only label users for reporting. The
// helpers below are top-level so web/budgets.test.mjs can extract them.

// fmtCompact shortens a count for tight cells: 950, 12.5K, 1.2M, 3B.
function fmtCompact(n) {
  const v = Number(n) || 0, a = Math.abs(v);
  const f = (x, s) => (Math.round(x * 10) / 10).toLocaleString("en-US") + s;
  if (a >= 999.95e6) return f(v / 1e9, "B");
  if (a >= 999.95e3) return f(v / 1e6, "M");
  if (a >= 1e3) return f(v / 1e3, "K");
  return String(Math.round(v));
}

// fmtZoned renders an instant as wall time in an IANA zone, the shape the
// gateway's budget 429 uses: "2026-12-01 00:00 UTC". An unknown zone falls
// back to UTC (and says so) rather than to the browser's own zone.
function fmtZoned(iso, tz) {
  const d = new Date(iso);
  if (!iso || isNaN(d)) return "—";
  for (const zone of [tz || "UTC", "UTC"]) {
    let parts;
    try {
      parts = new Intl.DateTimeFormat("en-US", { timeZone: zone, year: "numeric", month: "2-digit", day: "2-digit",
        hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).formatToParts(d);
    } catch (_) { continue; }
    const p = {};
    parts.forEach((x) => { p[x.type] = x.value; });
    return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute} ${zone}`;
  }
  return d.toISOString();
}

// fmtPeriodRange renders a report period: "2026-11-01 00:00 – 2026-12-01 00:00 UTC".
function fmtPeriodRange(start, end, tz) {
  return fmtZoned(start, tz).replace(/ \S+$/, "") + " – " + fmtZoned(end, tz);
}

// budgetPct is the share of a cap used, in percent, or null when there is
// no cap (limit 0 or missing). Not clamped: cost is known only after a
// response, so one request may overshoot the cap.
function budgetPct(used, limit) {
  const l = Number(limit) || 0;
  if (l <= 0) return null;
  return ((Number(used) || 0) * 100) / l;
}

// budgetLevel is the highlight class for a percent: "over" from 100,
// "warn" from 80, "" below that or without a cap.
function budgetLevel(pct) {
  if (pct == null) return "";
  if (pct >= 100) return "over";
  return pct >= 80 ? "warn" : "";
}

// budgetBars turns the budget block of GET /api/me into one entry per
// capped period and unit (day, week, month; tokens before cost). Empty
// when nothing is capped, so the dashboard shows no budget section.
function budgetBars(budget) {
  const periods = (budget && budget.periods) || {};
  const out = [];
  ["day", "week", "month"].forEach((period) => {
    const x = periods[period];
    if (!x) return;
    [["tokens", x.tokens_used, x.tokens_limit], ["cost_usd", x.cost_usd_used, x.cost_usd_limit]].forEach(([unit, used, limit]) => {
      const pct = budgetPct(used, limit);
      if (pct == null) return;
      out.push({ period, unit, used: Number(used) || 0, limit: Number(limit), pct, level: budgetLevel(pct), end: x.end });
    });
  });
  return out;
}

// monthSpend summarizes an admin users-list row for its spend column: month
// cost against the month cost cap ("$12.40 / $50.00", or just the spend
// without a cap), a tokens line when a month token cap is set, and the
// highlight level of whichever of the two is closer to its cap.
function monthSpend(u) {
  const s = (u && u.spend && u.spend.month) || {};
  const b = (u && u.budgets) || {};
  const capCost = Number(b.cost_usd && b.cost_usd.month) || 0;
  const capTok = Number(b.tokens && b.tokens.month) || 0;
  // spend: null = the server couldn't compute it; say so instead of "$0".
  if (u && u.spend === null) {
    return { cost: "—" + (capCost > 0 ? " / " + fmtUSD(capCost) : ""), tokens: capTok > 0 ? `— / ${fmtCompact(capTok)} tok` : "", level: budgetLevel(null) };
  }
  const pc = budgetPct(s.cost_usd, capCost), pt = budgetPct(s.tokens, capTok);
  const worst = pc == null ? pt : (pt == null ? pc : Math.max(pc, pt));
  return {
    cost: fmtUSD(s.cost_usd) + (capCost > 0 ? " / " + fmtUSD(capCost) : ""),
    tokens: capTok > 0 ? `${fmtCompact(s.tokens)} / ${fmtCompact(capTok)} tok` : "",
    level: budgetLevel(worst),
  };
}

// parseBudgets reads the user editor's budget grid — raw cell strings keyed
// "tokens.day" … "cost_usd.month" — into the API's budgets object. Every
// cell is sent and an empty one is 0 (no cap), so clearing a cell clears
// the stored cap. Throws on a negative or non-numeric value and on a
// fractional token count.
function parseBudgets(cells) {
  const out = { tokens: {}, cost_usd: {} };
  for (const unit of ["tokens", "cost_usd"]) {
    for (const period of ["day", "week", "month"]) {
      const t = String((cells || {})[unit + "." + period] ?? "").trim();
      const v = t === "" ? 0 : Number(t);
      const name = (unit === "tokens" ? "Tokens" : "Cost $") + " per " + period;
      if (!Number.isFinite(v) || v < 0) throw new Error(`${name} must be a non-negative number`);
      if (unit === "tokens" && !Number.isInteger(v)) throw new Error(`${name} must be a whole number`);
      out[unit][period] = v;
    }
  }
  return out;
}

// orderGroupSpend keeps the API's cost-descending order of GET
// /api/admin/usage/groups but moves Ungrouped (group_id null) last.
function orderGroupSpend(groups) {
  const list = groups || [];
  return list.filter((g) => g.group_id != null).concat(list.filter((g) => g.group_id == null));
}

// liveSummary rolls the Live page's in-flight requests up per user: in
// flight count, the user's current group and today's spend from
// spendBySubject ({subject: {tokens, cost_usd}}; null while not loaded, so
// spend is null = unknown). Users are keyed by subject (user) and carry the
// label shown for them (user_label, else the subject). A request without an owner (a key with no
// user) counts under its key name, ungrouped and without spend, so the
// in-flight counts always add up to the table below. groups sums those
// active users per group, Ungrouped ("") last; its spend is null when no
// member's spend is known.
function liveSummary(requests, spendBySubject) {
  const users = [], byKey = Object.create(null);
  (requests || []).forEach((r) => {
    const key = r.user ? "u:" + r.user : "k:" + (r.key_name || r.key_id || "");
    let u = byKey[key];
    if (!u) {
      const sp = r.user && spendBySubject && Object.prototype.hasOwnProperty.call(spendBySubject, r.user) ? spendBySubject[r.user] : null;
      u = byKey[key] = { user: r.user || "", label: r.user ? (r.user_label || r.user) : "", key_name: r.user ? "" : (r.key_name || r.key_id || ""), group: r.group || "",
        inflight: 0, tokens: sp ? Number(sp.tokens) || 0 : null, cost_usd: sp ? Number(sp.cost_usd) || 0 : null };
      users.push(u);
    }
    u.inflight++;
  });
  const name = (u) => u.label || u.key_name;
  users.sort((a, b) => b.inflight - a.inflight || name(a).localeCompare(name(b)));
  const groups = [], byGroup = Object.create(null);
  users.forEach((u) => {
    let g = byGroup[u.group];
    if (!g) { g = byGroup[u.group] = { group: u.group, users: 0, inflight: 0, tokens: null, cost_usd: null }; groups.push(g); }
    g.users++;
    g.inflight += u.inflight;
    if (u.tokens != null) { g.tokens = (g.tokens || 0) + u.tokens; g.cost_usd = (g.cost_usd || 0) + u.cost_usd; }
  });
  groups.sort((a, b) => (a.group === "") - (b.group === "") || b.inflight - a.inflight || a.group.localeCompare(b.group));
  return { users, groups };
}

// ---------- live (admin) ----------
// Requests in flight right now, from GET /api/admin/live. Polled every 2s
// while the page is visible and not paused; elapsed times tick client-side
// between polls from each row's server-side elapsed_ms.

// fmtElapsed renders a duration in ms compactly: 4.2s, 1m 05s, 2h 03m.
// Top-level so web/live.test.mjs can extract it (as the helpers below).
function fmtElapsed(ms) {
  const v = Math.max(0, Number(ms) || 0);
  if (v < 60000) return (v / 1000).toFixed(1) + "s";
  const s = Math.floor(v / 1000);
  const pad = (n) => String(n).padStart(2, "0");
  if (s < 3600) return `${Math.floor(s / 60)}m ${pad(s % 60)}s`;
  return `${Math.floor(s / 3600)}h ${pad(Math.floor(s / 60) % 60)}m`;
}

// fmtBytes renders a byte count: 812 B, 47.1 KB, 3.2 MB.
function fmtBytes(n) {
  const v = Math.max(0, Number(n) || 0);
  if (v < 1024) return `${v} B`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
  return `${(v / 1024 / 1024).toFixed(1)} MB`;
}

// userLabelHTML renders a user for the console: the human label (display
// name, else email; the API falls back to the subject) with the subject
// (the stable id) in a tooltip. A label that is missing (an older pod or
// report) or equal to the id shows just the id.
function userLabelHTML(id, label) {
  const shown = label || id || "";
  return shown === (id || "") ? esc(shown) : `<span title="${esc(id)}">${esc(shown)}</span>`;
}

// liveOutputHTML renders a live request's Output cell. A unary request
// learns its tokens and bytes only when the response completes, so with
// nothing out yet it reads as waiting rather than as zeros; a stream shows
// its running counts.
function liveOutputHTML(r) {
  if (!r.stream && !Number(r.output_tokens) && !Number(r.bytes_out)) return `<span class="muted waiting">waiting for response</span>`;
  return `${fmtNum(r.output_tokens)} tok<div class="sub">${fmtBytes(r.bytes_out)}</div>`;
}

// groupLive groups in-flight requests that share a session_id. Returns a
// list in the order of each item's first request (the API sorts by
// started_at): {session_id, requests} for a session with 2+ requests, and
// {session_id: "", requests: [r]} for everything else.
function groupLive(requests) {
  const list = requests || [];
  const count = Object.create(null);
  list.forEach((r) => { if (r.session_id) count[r.session_id] = (count[r.session_id] || 0) + 1; });
  const out = [], bySession = Object.create(null);
  list.forEach((r) => {
    if (r.session_id && count[r.session_id] > 1) {
      if (!bySession[r.session_id]) { bySession[r.session_id] = { session_id: r.session_id, requests: [] }; out.push(bySession[r.session_id]); }
      bySession[r.session_id].requests.push(r);
    } else {
      out.push({ session_id: "", requests: [r] });
    }
  });
  return out;
}

// stalePods returns the pods whose last heartbeat is more than maxAgeMs
// older than the snapshot, each with its age in ms.
function stalePods(pods, generatedAt, maxAgeMs) {
  const at = Date.parse(generatedAt);
  if (isNaN(at)) return [];
  return (pods || [])
    .map((p) => ({ pod: p.pod, age_ms: at - Date.parse(p.seen_at) }))
    .filter((p) => p.age_ms > maxAgeMs);
}

// spend is today's spend per user subject from the admin users list,
// refreshed every 15s from the poll loop (not every 2s); spendErr holds the
// last failure; reqs is the last snapshot, so the summary can redraw when
// spend arrives.
const live = { timer: null, tick: null, paused: false, fetchedAt: 0, spend: null, spendAt: 0, spendErr: "", reqs: [] };

function stopLive() {
  clearTimeout(live.timer);
  clearInterval(live.tick);
  live.timer = live.tick = null;
  document.removeEventListener("visibilitychange", onLiveVisibility);
}

function onLiveVisibility() {
  if (!document.hidden && !live.paused && !live.timer) pollLive();
}

function viewLive(view) {
  live.paused = false;
  live.spend = null;
  live.spendAt = 0;
  live.spendErr = "";
  live.reqs = [];
  view.innerHTML = `<div class="page-head"><h1 class="page-title">Live</h1>
      <div class="row"><span class="muted" id="live-status"></span>
        <button class="btn ghost sm" id="live-pause" type="button">Pause</button></div></div>
    <div id="live-root"><div class="empty">Loading…</div></div>`;
  $("#live-pause").addEventListener("click", () => {
    live.paused = !live.paused;
    $("#live-pause").textContent = live.paused ? "Resume" : "Pause";
    if (live.paused) { clearTimeout(live.timer); live.timer = null; liveStatus(); } else pollLive();
  });
  document.addEventListener("visibilitychange", onLiveVisibility);
  live.tick = setInterval(tickLive, 1000);
  pollLive();
}

function liveStatus() {
  const el = $("#live-status");
  if (!el) return;
  if (live.paused) el.textContent = "Paused";
  else if (document.hidden) el.textContent = "Paused while hidden";
  else el.textContent = live.fetchedAt ? `Updated ${new Date(live.fetchedAt).toLocaleTimeString()} · every 2s` : "";
}

async function pollLive() {
  clearTimeout(live.timer);
  live.timer = null;
  if (live.paused || document.hidden) { liveStatus(); return; }
  const r = await api("GET", "/api/admin/live");
  const root = $("#live-root");
  if (!root || live.paused) return; // navigated away or paused meanwhile
  // A failed poll stops polling until Retry, rather than hammering a
  // backend that is already failing.
  if (loadFailed(root, [r], pollLive)) { liveStatus(); return; }
  live.fetchedAt = Date.now();
  renderLive(root, r.data || {});
  liveStatus();
  if (Date.now() - live.spendAt >= 15000) refreshLiveSpend();
  if (!document.hidden) live.timer = setTimeout(pollLive, 2000);
}

// refreshLiveSpend reloads today's per-user spend for the summary. A
// failure only blanks the spend columns (with a Retry); the in-flight view
// keeps polling.
async function refreshLiveSpend() {
  live.spendAt = Date.now();
  const r = await api("GET", "/api/admin/users");
  if (!$("#live-root")) return; // navigated away meanwhile
  if (r.ok) {
    live.spend = Object.create(null);
    ((r.data && r.data.users) || []).forEach((u) => { if (u.spend) live.spend[u.subject] = u.spend.day || { tokens: 0, cost_usd: 0 }; });
    live.spendErr = "";
  } else {
    live.spend = null;
    live.spendErr = apiError(r);
  }
  renderLiveSummary();
}

// renderLiveSummary draws "who is active now" above the request table:
// one row per user with requests in flight and a roll-up of those users
// per group.
function renderLiveSummary() {
  const el = $("#live-summary");
  if (!el) return;
  if (!live.reqs.length) { el.innerHTML = ""; return; }
  const s = liveSummary(live.reqs, live.spend);
  const tok = (v) => (v == null ? `<span class="muted">—</span>` : `<span title="${esc(fmtNum(v))}">${fmtCompact(v)}</span>`);
  const usd = (v) => (v == null ? `<span class="muted">—</span>` : fmtUSD(v));
  const grp = (g) => (g ? esc(g) : `<span class="muted">Ungrouped</span>`);
  // On a phone the group column folds into a line under the user (the
  // "m-only" line), so the numbers stay on screen.
  const userRows = s.users.map((u) => `<tr>
    <td class="name">${u.user ? userLabelHTML(u.user, u.label) : `<span class="mono">${esc(u.key_name)}</span> <span class="muted">(no owner)</span>`}
      <div class="sub m-only">${grp(u.group)}</div></td>
    <td class="opt">${grp(u.group)}</td><td class="num">${fmtNum(u.inflight)}</td><td class="num">${tok(u.tokens)}</td><td class="num">${usd(u.cost_usd)}</td></tr>`).join("");
  const groupRows = s.groups.map((g) => `<tr>
    <td class="name">${grp(g.group)}</td><td class="num opt">${fmtNum(g.users)}</td><td class="num">${fmtNum(g.inflight)}</td><td class="num">${tok(g.tokens)}</td><td class="num">${usd(g.cost_usd)}</td></tr>`).join("");
  const err = live.spendErr ? `<div class="load-error-body"><p class="err-text">Today's spend unavailable: ${esc(live.spendErr)}</p>
    <button class="btn sm" type="button" id="live-spend-retry">Retry</button></div>` : "";
  const table = (cols, rows) => `<div class="table-scroll"><table class="summary-table"><thead><tr>${cols.map(([c, cls]) => `<th class="${cls}">${c}</th>`).join("")}</tr></thead><tbody>${rows}</tbody></table></div>`;
  const named = s.users.filter((u) => u.user).length, keys = s.users.length - named;
  const head = `${named} user${named === 1 ? "" : "s"}${keys ? ` · ${keys} ownerless key${keys === 1 ? "" : "s"}` : ""}`;
  el.innerHTML = `<div class="live-summary">
    <div class="panel"><div class="panel-head"><h2>Active now · ${head}</h2></div>${err}
      ${table([["User", ""], ["Group", "opt"], ["In flight", "num"], ["Tokens today", "num"], ["Cost today", "num"]], userRows)}</div>
    <div class="panel"><div class="panel-head"><h2>By group · active users</h2></div>
      ${table([["Group", ""], ["Users", "num opt"], ["In flight", "num"], ["Tokens today", "num"], ["Cost today", "num"]], groupRows)}</div>
  </div>`;
  const retry = $("#live-spend-retry");
  if (retry) retry.addEventListener("click", refreshLiveSpend);
}

function tickLive() {
  if (live.paused || !live.fetchedAt) return;
  const delta = Date.now() - live.fetchedAt;
  document.querySelectorAll("#live-root [data-elapsed]").forEach((el) => {
    el.textContent = fmtElapsed(Number(el.getAttribute("data-elapsed")) + delta);
  });
}

function renderLive(root, d) {
  const reqs = d.requests || [];
  const pods = d.pods || [];
  const stale = stalePods(pods, d.generated_at, 10000);
  const phaseBadge = { routing: "neutral", upstream: "admin", streaming: "active" };
  const row = (r, inGroup) => `<tr class="${inGroup ? "in-group" : ""}${r.attempt > 1 ? " failover" : ""}">
    <td class="mono num" data-elapsed="${Number(r.elapsed_ms) || 0}">${fmtElapsed(r.elapsed_ms)}</td>
    <td class="user">${r.user ? userLabelHTML(r.user, r.user_label) : `<span class="muted">—</span>`}<div class="sub mono">${esc(r.key_name || r.key_id)}</div></td>
    <td class="mono">${esc(r.endpoint)}${r.stream ? ` <span class="badge neutral">stream</span>` : ""}</td>
    <td class="mono wrap">${esc(r.alias)}<div class="sub">${r.provider ? `→ ${esc(r.provider)}/${esc(r.upstream_model)}` : `<span class="muted">not routed yet</span>`}</div></td>
    <td><span class="badge ${phaseBadge[r.phase] || "neutral"}">${esc(r.phase)}</span></td>
    <td>${r.attempt > 1 ? `<span class="badge warn" title="failed over to attempt ${Number(r.attempt)}">#${Number(r.attempt)} failover</span>` : Number(r.attempt) || 1}</td>
    <td class="mono num">${liveOutputHTML(r)}</td>
    <td class="mono" title="${esc(r.pod)}">${esc(String(r.pod || "").split("-").pop())}</td>
    <td class="mono">${esc(r.client_ip)}</td></tr>`;
  const body = groupLive(reqs).map((g) => g.session_id
    ? `<tr class="session-head"><td colspan="9"><span class="badge neutral">session</span>
        <span class="mono" title="${esc(g.session_id)}">${esc(g.session_id)}</span>
        <span class="muted">· ${g.requests.length} requests in flight · ${g.requests[0].user ? userLabelHTML(g.requests[0].user, g.requests[0].user_label) : esc(g.requests[0].key_name || "")}</span></td></tr>`
      + g.requests.map((r) => row(r, true)).join("")
    : row(g.requests[0], false)).join("");
  const notes = [];
  if (d.partial) notes.push(`<div class="notice warn">Partial view: the shared store is unreachable, so only this pod's own requests are shown.</div>`);
  if (stale.length) notes.push(`<div class="notice warn">No recent heartbeat from ${stale.map((p) => `<span class="mono">${esc(p.pod)}</span> (${fmtElapsed(p.age_ms)} ago)`).join(", ")} — its requests may be missing or finished.</div>`);
  const podChips = pods.map((p) => `<span class="pod-chip${stale.some((s) => s.pod === p.pod) ? " stale" : ""}"><span class="mono">${esc(p.pod)}</span> · ${fmtNum(p.inflight)}</span>`).join("");
  live.reqs = reqs;
  root.innerHTML = notes.join("") + `<div id="live-summary"></div><div class="panel">
    <div class="panel-head"><h2>${reqs.length} in flight</h2><div class="pod-chips">${podChips}</div></div>
    ${reqs.length === 0 ? `<div class="empty">No requests in flight</div>` : `<div class="table-scroll"><table class="live-table">
      <thead><tr>${["Elapsed", "User / key", "Endpoint", "Alias → upstream", "Phase", "Attempt", "Output", "Pod", "Client IP"].map((c) => `<th>${c}</th>`).join("")}</tr></thead>
      <tbody>${body}</tbody></table></div>`}
  </div>`;
  renderLiveSummary();
}

// ---------- keys ----------
async function viewKeys(view) {
  view.innerHTML = `
    <h1 class="page-title">API Keys</h1>
    <div class="row" style="margin-bottom:1rem">
      <input id="key-name" placeholder="Key name (e.g. cursor laptop)" style="max-width:280px" />
      <button class="btn" id="key-create">Create key</button>
    </div>
    <div id="reveal"></div>
    <div id="keys-panel"></div>
    ${connectPanel()}`;
  $("#key-create").addEventListener("click", createKey);
  $("#key-name").addEventListener("keydown", (e) => { if (e.key === "Enter") createKey(); });
  await loadKeys();
}

const keysGuard = staleGuard();
async function loadKeys() {
  const token = keysGuard.start();
  const r = await api("GET", "/api/keys");
  if (!keysGuard.current(token)) return;
  if (loadFailed($("#keys-panel"), [r], loadKeys)) return;
  const keys = (r.data && r.data.keys) || [];
  $("#keys-panel").innerHTML = `
    <div class="panel">
      <div class="panel-head"><h2>${keys.length} key${keys.length === 1 ? "" : "s"}</h2></div>
      ${keys.length === 0 ? `<div class="empty">No keys yet. Create one above.</div>` : `
      <table><thead><tr><th>Name</th><th>Key</th><th>Status</th><th>Created</th><th>Last used</th><th></th></tr></thead>
      <tbody>${keys.map(keyRow).join("")}</tbody></table>`}
    </div>`;
  document.querySelectorAll("[data-revoke]").forEach((b) =>
    b.addEventListener("click", () => revokeKey(b.getAttribute("data-revoke"), b.getAttribute("data-name"))));
}

function keyRow(k) {
  const revoke = k.status === "active"
    ? `<button class="btn danger sm" data-revoke="${esc(k.id)}" data-name="${esc(k.name)}">Revoke</button>` : "";
  return `<tr>
    <td>${esc(k.name)}</td>
    <td class="mono">${esc(k.prefix)}…${esc(k.last4)}</td>
    <td><span class="badge ${esc(k.status)}">${esc(k.status)}</span></td>
    <td>${fmtTime(k.created_at)}</td>
    <td>${fmtTime(k.last_used_at)}</td>
    <td style="text-align:right">${revoke}</td></tr>`;
}

async function createKey() {
  const name = ($("#key-name").value || "").trim() || "key";
  const r = await api("POST", "/api/keys", { name });
  if (!r.ok) { toast((r.data && r.data.error) || "Failed to create key", "err"); return; }
  $("#key-name").value = "";
  $("#reveal").innerHTML = `
    <div class="panel" style="border-color:var(--accent)">
      <div class="panel-head"><h2>New key — copy it now</h2></div>
      <div style="padding:1rem 1.1rem">
        <div class="token-box" id="tok">${esc(r.data.token)}</div>
        <div class="row">
          <button class="btn sm" id="copy-tok">Copy</button>
          <span class="token-note">This token is shown only once.</span>
        </div>
      </div>
    </div>`;
  $("#copy-tok").addEventListener("click", () => {
    // navigator.clipboard is undefined on an insecure (plain-HTTP) origin —
    // a real deployment mode here, TLS is an opt-in Caddy profile, not the
    // default — and writeText can also reject on permission denial. Either
    // way the token stays visible above for a manual copy, so this is a
    // clear message, not a stuck UI.
    if (!navigator.clipboard) {
      toast("Clipboard unavailable here (needs HTTPS) — copy the token above by hand", "err");
      return;
    }
    navigator.clipboard.writeText(r.data.token)
      .then(() => toast("Copied to clipboard"))
      .catch(() => toast("Couldn't copy — copy the token above by hand", "err"));
  });
  toast("Key created");
  loadKeys();
}

async function revokeKey(id, name) {
  if (!confirm(`Revoke key "${name}"? Clients using it will stop working.`)) return;
  const r = await api("POST", `/api/keys/${id}/revoke`);
  if (r.ok) { toast("Key revoked"); loadKeys(); }
  else toast((r.data && r.data.error) || "Failed to revoke", "err");
}

// ---------- admin ----------
function viewAdmin(view, tab) {
  const tabs = ADMIN_TABS.map((t) =>
    `<button class="${t === tab ? "active" : ""}" onclick="location.hash='#/admin/${t}'">${t}</button>`).join("");
  view.innerHTML = `<h1 class="page-title">Admin console</h1><div class="tabs">${tabs}</div><div id="atab"></div>`;
  const c = $("#atab");
  ({
    users: adminUsers, groups: adminGroups, keys: adminKeys, usage: adminUsage, roles: adminRoles,
    aliases: adminAliases, providers: adminProviders, pricing: adminPricing,
    dlp: adminDLP, audit: adminAudit,
  }[tab] || adminUsers)(c);
}

async function adminUsage(c) {
  const [u, b, r] = await Promise.all([
    api("GET", "/api/admin/usage"),
    api("GET", "/api/admin/usage/breakdown?hours=24"),
    api("GET", "/api/admin/usage/recent?limit=50"),
  ]);
  if (loadFailed(c, [u, b, r], () => adminUsage(c))) return;
  c.innerHTML = `<h2 style="font-size:1.05rem">Organization usage</h2>` + usageCards(u.data || {}) +
    breakdownTables(b.data || {}) + recentRequestsTable((r.data || {}).requests || []);
}

// tokensOut renders an output token count with the reasoning share in
// parentheses when there is one. The share is PART OF the output count, not an
// extra alongside it — Google and OpenAI both bill thinking at the output rate
// — so it reads as an annotation rather than a second number, and a row from a
// model that did not think looks exactly as it always did.
function tokensOut(out, reasoning) {
  const tok = (n) => (+n || 0).toLocaleString("en-US");
  return +reasoning > 0 ? `${tok(out)} (${tok(reasoning)} reasoning)` : tok(out);
}

// recentRequestsTable renders the last N ledger rows, newest first — a raw
// per-request tail (status, latency, error) alongside the aggregates above.
function recentRequestsTable(reqs) {
  const tok = (n) => (+n || 0).toLocaleString("en-US");
  const rows = reqs.map((x) => `<tr class="${x.status >= 400 ? "err-row" : ""}">
    <td class="mono nw">${esc(new Date(x.ts).toLocaleString())}</td>
    <td class="mono">${esc(x.alias)}</td>
    <td>${esc(x.provider) || "(none)"}</td>
    <td class="mono">${esc(x.upstream_model)}</td>
    <td>${x.status}</td>
    <td class="nw">${fmtNum(x.latency_ms)} ms</td>
    <td class="mono nw">${tok(x.tokens_in)} / ${tokensOut(x.tokens_out, x.tokens_reasoning)}</td>
    <td class="nw">${fmtUSD(x.cost_usd)}</td>
    <td class="mono trunc" title="${esc(x.error || "")}">${esc(x.error || "")}</td></tr>`);
  return panelTable("Recent requests",
    ["Time", "Alias", "Provider", "Upstream model", "Status", "Latency", "Tokens in / out", "Cost", "Error"], rows);
}

async function adminUsers(c) {
  const r = await api("GET", "/api/admin/users");
  if (loadFailed(c, [r], () => adminUsers(c))) return;
  const users = (r.data && r.data.users) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-user">New user</button></div>` +
    panelTable("Users", ["Subject", "Email", "Group", "Roles", "Month spend", "Auth", "Status", "Created", ""],
      users.map((u) => {
        const ms = monthSpend(u);
        return `<tr>
        <td>${esc(u.subject)}</td>
        <td class="email-cell">${esc(u.email)}</td>
        <td class="trunc group-cell" title="${esc(u.group_name)}">${u.group_name ? esc(u.group_name) : `<span class="muted">—</span>`}</td>
        <td>${(u.roles || []).map((x) => `<span class="badge ${x === "airllm_admin" ? "admin" : "neutral"}">${esc(x)}</span>`).join(" ")}</td>
        <td class="nw"><span class="spend ${ms.level}">${esc(ms.cost)}</span>${ms.tokens ? `<div class="sub spend ${ms.level}">${esc(ms.tokens)}</div>` : ""}</td>
        <td>${esc(u.auth_source || "local")}</td>
        <td>${u.disabled ? `<span class="badge revoked">disabled</span>` : `<span class="badge active">active</span>`}</td>
        <td>${fmtTime(u.created_at)}</td>
        <td class="actions">
          <button class="btn ghost sm" data-edit-user='${esc(JSON.stringify(u))}'>Edit</button>
          ${u.auth_source === "oidc" ? "" : `<button class="btn ghost sm" data-reset-user="${esc(u.id)}">Reset pw</button>`}
          ${u.subject === me.subject ? "" : `<button class="btn danger sm" data-del-user="${esc(u.id)}" data-name="${esc(u.subject)}">Delete</button>`}
        </td></tr>`;
      }));
  $("#new-user").addEventListener("click", () => editUser(c, null));
  document.querySelectorAll("[data-edit-user]").forEach((b) =>
    b.addEventListener("click", () => editUser(c, JSON.parse(b.getAttribute("data-edit-user")))));
  document.querySelectorAll("[data-reset-user]").forEach((b) =>
    b.addEventListener("click", () => resetUserPassword(c, b.getAttribute("data-reset-user"))));
  document.querySelectorAll("[data-del-user]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm(`Delete user "${b.getAttribute("data-name")}"?`)) return;
    const x = await api("DELETE", `/api/admin/users/${encodeURIComponent(b.getAttribute("data-del-user"))}`);
    if (x.ok) { toast("User deleted"); adminUsers(c); }
    else toast((x.data && x.data.error) || "Failed", "err");
  }));
}

async function editUser(c, u) {
  const [rolesR, groupsR] = await Promise.all([api("GET", "/api/admin/roles"), api("GET", "/api/admin/groups")]);
  if (!rolesR.ok) { toast("Could not load roles: " + apiError(rolesR), "err"); return; }
  if (!groupsR.ok) { toast("Could not load groups: " + apiError(groupsR), "err"); return; }
  const groupOptions = [{ value: "", label: "No group" },
    ...((groupsR.data && groupsR.data.groups) || []).map((g) => ({ value: g.id, label: g.name }))];
  const tz = me.budget && me.budget.tz;
  const availRoles = ((rolesR.data && rolesR.data.roles) || []).map((rp) => rp.role);
  // Roles are picked, not typed, so a typo can't be saved silently. A role
  // the user already holds stays listed even without a policy row.
  const roleOptions = [...new Set([...availRoles, ...((u && u.roles) || [])])];
  const isNew = !u;
  const fields = [];
  if (isNew) fields.push({ name: "username", label: "Username", value: "" });
  fields.push(
    { name: "email", label: "Email", value: u ? (u.email || "") : "" },
    { name: "display", label: "Display name", value: u ? (u.display || "") : "" },
    { name: "roles", label: "Roles", type: "checkgroup", options: roleOptions, value: u ? (u.roles || []) : [] },
    { name: "disabled", label: "Disabled", type: "checkbox", value: u ? !!u.disabled : false },
    { name: "group_id", label: "Group", type: "select", options: groupOptions, value: u ? (u.group_id || "") : "" },
    { name: "budgets", type: "budgets", value: u ? (u.budgets || {}) : {},
      label: `Budget per calendar period${tz ? ` (${tz})` : ""} — empty = no cap` },
  );
  if (isNew) fields.push({ name: "password", label: "Password", type: "password", value: "" });
  modalForm(isNew ? "New user" : `Edit user ${u.subject}`, fields, async (v) => {
    const roles = v.roles;
    let budgets;
    try { budgets = parseBudgets(v.budgets); } catch (e) { toast(e.message, "err"); return false; }
    // Sent explicitly (null = no group, 0 = no cap): the update keeps the
    // stored value of any field it omits.
    const groupID = v.group_id || null;
    let x;
    if (isNew) {
      x = await api("POST", "/api/admin/users", {
        username: v.username, email: v.email, display: v.display, roles, password: v.password,
      });
      if (!x.ok) { toast((x.data && x.data.error) || "Failed", "err"); return false; }
      // Create answers only {id}; group and budgets go in a follow-up
      // update, skipped when nothing is set.
      const capped = Object.values(budgets).some((per) => Object.values(per).some((n) => n > 0));
      if (groupID || capped) {
        const y = await api("PUT", `/api/admin/users/${encodeURIComponent(x.data.id)}`, { group_id: groupID, budgets });
        if (!y.ok) {
          toast(`User ${v.username} created, but group and budget were not saved: ${apiError(y)} — edit the user to set them`, "err");
          adminUsers(c);
          return true;
        }
      }
      toast("User created"); adminUsers(c); return true;
    }
    x = await api("PUT", `/api/admin/users/${encodeURIComponent(u.id)}`, {
      email: v.email, display: v.display, roles, disabled: v.disabled, group_id: groupID, budgets,
    });
    if (x.ok) { toast("User saved"); adminUsers(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

function resetUserPassword(_c, id) {
  modalForm("Reset password", [
    { name: "password", label: "New password", type: "password", value: "" },
  ], async (v) => {
    if (!v.password) { toast("Password is required", "err"); return false; }
    const x = await api("POST", `/api/admin/users/${encodeURIComponent(id)}/password`, { password: v.password });
    if (x.ok) { toast("Password reset"); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

// ---------- groups (admin) ----------
// User groups are labels for reporting: CRUD on top, spend per group for
// the current day / week / month below, refreshed every 15s while visible.
// The selected period and the expanded groups survive refreshes.
const gspend = { timer: null, period: "month", open: new Set(), guard: staleGuard() };

function stopGroupSpend() {
  clearTimeout(gspend.timer);
  gspend.timer = null;
  document.removeEventListener("visibilitychange", onGroupSpendVisibility);
}

function onGroupSpendVisibility() {
  if (!document.hidden && !gspend.timer) loadGroupSpend();
}

async function adminGroups(c) {
  stopGroupSpend();
  const r = await api("GET", "/api/admin/groups");
  if (loadFailed(c, [r], () => adminGroups(c))) return;
  const groups = (r.data && r.data.groups) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-group">New group</button></div>` +
    panelTable("Groups", ["Name", "Description", "Members", ""],
      groups.map((g) => `<tr>
        <td>${esc(g.name)}</td>
        <td class="wrap-cell">${esc(g.description) || `<span class="muted">—</span>`}</td>
        <td class="num">${fmtNum(g.member_count)}</td>
        <td class="actions">
          <button class="btn ghost sm" data-edit-group='${esc(JSON.stringify(g))}'>Edit</button>
          <button class="btn danger sm" data-del-group="${esc(g.id)}" data-name="${esc(g.name)}" data-members="${Number(g.member_count) || 0}">Delete</button>
        </td></tr>`)) +
    `<div id="group-spend"><div class="empty">Loading…</div></div>`;
  $("#new-group").addEventListener("click", () => editGroup(c, null));
  c.querySelectorAll("[data-edit-group]").forEach((b) =>
    b.addEventListener("click", () => editGroup(c, JSON.parse(b.getAttribute("data-edit-group")))));
  c.querySelectorAll("[data-del-group]").forEach((b) => b.addEventListener("click", async () => {
    const name = b.getAttribute("data-name"), n = Number(b.getAttribute("data-members"));
    const who = n ? ` Its ${n} member${n === 1 ? "" : "s"} become${n === 1 ? "s" : ""} ungrouped.` : "";
    if (!confirm(`Delete group "${name}"?${who}`)) return;
    const x = await api("DELETE", `/api/admin/groups/${encodeURIComponent(b.getAttribute("data-del-group"))}`);
    if (x.ok) { toast(`Group ${name} deleted`); adminGroups(c); }
    else toast(apiError(x), "err");
  }));
  document.addEventListener("visibilitychange", onGroupSpendVisibility);
  loadGroupSpend();
}

function editGroup(c, g) {
  modalForm(g ? `Edit group ${g.name}` : "New group", [
    { name: "name", label: "Name", value: g ? g.name : "" },
    { name: "description", label: "Description", value: g ? (g.description || "") : "" },
  ], async (v) => {
    const body = { name: v.name.trim(), description: v.description.trim() };
    if (!body.name) { toast("Name is required", "err"); return false; }
    const x = g
      ? await api("PUT", `/api/admin/groups/${encodeURIComponent(g.id)}`, body)
      : await api("POST", "/api/admin/groups", body);
    if (x.ok) { toast(g ? "Group saved" : "Group created"); adminGroups(c); return true; }
    toast(x.status === 409 ? `A group named "${body.name}" already exists` : apiError(x), "err");
    return false;
  });
}

async function loadGroupSpend() {
  clearTimeout(gspend.timer);
  gspend.timer = null;
  if (!$("#group-spend") || document.hidden) return; // resumes on visibilitychange
  const token = gspend.guard.start();
  const r = await api("GET", `/api/admin/usage/groups?period=${encodeURIComponent(gspend.period)}`);
  const el = $("#group-spend");
  if (!gspend.guard.current(token) || !el) return;
  // A failed load stops refreshing until Retry.
  if (loadFailed(el, [r], loadGroupSpend)) return;
  renderGroupSpend(el, r.data || {});
  gspend.timer = setTimeout(loadGroupSpend, 15000);
}

function renderGroupSpend(el, d) {
  const groups = orderGroupSpend(d.groups);
  const total = d.total || {};
  const tok = (n) => `<span title="${esc(fmtNum(n))}">${fmtCompact(n)}</span>`;
  const periods = [["day", "Day"], ["week", "Week"], ["month", "Month"]];
  const rows = groups.map((g) => {
    const key = g.group_id == null ? "ungrouped" : String(g.group_id);
    const open = gspend.open.has(key);
    const users = g.users || [];
    const name = g.group_id == null ? `<span class="muted">${esc(g.name || "Ungrouped")}</span>` : esc(g.name);
    return `<tr class="group-row${open ? " open" : ""}">
        <td><button class="expander" type="button" data-gkey="${esc(key)}" aria-expanded="${open}"${users.length ? "" : " disabled"}>
          <span class="caret">${users.length ? "▸" : "·"}</span>${name}</button></td>
        <td class="num opt">${fmtNum(users.length)}</td><td class="num opt">${fmtNum(g.requests)}</td>
        <td class="num">${tok(g.tokens)}</td><td class="num">${fmtUSD(g.cost_usd)}</td></tr>` +
      (open ? users.map((u) => `<tr class="member-row">
        <td><span class="member">${u.subject ? userLabelHTML(u.subject, u.label) : `<span class="muted">deleted users</span>`}</span></td><td class="opt"></td><td class="num opt">${fmtNum(u.requests)}</td>
        <td class="num">${tok(u.tokens)}</td><td class="num">${fmtUSD(u.cost_usd)}</td></tr>`).join("") : "");
  }).join("");
  el.innerHTML = `<div class="panel">
    <div class="panel-head wrap"><h2>Spend by group</h2>
      <div class="seg" role="tablist">${periods.map(([p, l]) => `<button type="button" role="tab" data-period="${p}" aria-selected="${p === gspend.period}" class="${p === gspend.period ? "active" : ""}">${l}</button>`).join("")}</div></div>
    <div class="panel-sub">${esc(fmtPeriodRange(d.start, d.end, d.tz))} · refreshes every 15s</div>
    ${groups.length === 0 ? `<div class="empty">No usage in this period.</div>` : `<div class="table-scroll"><table class="group-table">
      <thead><tr><th>Group</th><th class="num opt">Users</th><th class="num opt">Requests</th><th class="num">Tokens</th><th class="num">Cost</th></tr></thead>
      <tbody>${rows}</tbody>
      <tfoot><tr><td>Total</td><td class="num opt">${fmtNum(groups.reduce((n, g) => n + (g.users || []).length, 0))}</td>
        <td class="num opt">${fmtNum(total.requests)}</td><td class="num">${tok(total.tokens)}</td><td class="num">${fmtUSD(total.cost_usd)}</td></tr></tfoot>
    </table></div>`}
  </div>`;
  el.querySelectorAll("[data-period]").forEach((b) => b.addEventListener("click", () => {
    if (b.getAttribute("data-period") === gspend.period) return;
    gspend.period = b.getAttribute("data-period");
    loadGroupSpend();
  }));
  el.querySelectorAll("[data-gkey]").forEach((b) => b.addEventListener("click", () => {
    const k = b.getAttribute("data-gkey");
    if (gspend.open.has(k)) gspend.open.delete(k); else gspend.open.add(k);
    renderGroupSpend(el, d);
  }));
}

async function adminKeys(c) {
  const r = await api("GET", "/api/admin/keys");
  if (loadFailed(c, [r], () => adminKeys(c))) return;
  const keys = (r.data && r.data.keys) || [];
  c.innerHTML = panelTable("All API keys", ["Owner", "Name", "Key", "Status", "Last used", ""],
    keys.map((k) => `<tr><td>${esc(k.owner)}</td><td>${esc(k.name)}</td>
      <td class="mono">${esc(k.prefix)}…${esc(k.last4)}</td>
      <td><span class="badge ${esc(k.status)}">${esc(k.status)}</span></td>
      <td>${fmtTime(k.last_used_at)}</td>
      <td class="actions">${k.status === "active" ? `<button class="btn danger sm" data-arev="${esc(k.id)}" data-name="${esc(k.name)}" data-owner="${esc(k.owner)}">Revoke</button>` : ""}</td></tr>`));
  document.querySelectorAll("[data-arev]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm(`Revoke key "${b.getAttribute("data-name")}" of ${b.getAttribute("data-owner")}?`)) return;
    const x = await api("POST", `/api/admin/keys/${b.getAttribute("data-arev")}/revoke`);
    if (x.ok) { toast("Revoked"); adminKeys(c); } else toast("Failed", "err");
  }));
}

async function adminAudit(c) {
  const r = await api("GET", "/api/admin/audit");
  if (loadFailed(c, [r], () => adminAudit(c))) return;
  const rows = (r.data && r.data.audit) || [];
  const detail = (d) => (d == null ? "" : JSON.stringify(d));
  c.innerHTML = panelTable("Audit log", ["Time", "Actor", "Action", "Target", "Detail"],
    rows.map((e) => `<tr><td>${fmtTime(e.ts)}</td><td>${esc(e.actor)}</td>
      <td class="mono">${esc(e.action)}</td><td class="mono">${esc(e.target)}</td>
      <td class="mono trunc" title="${esc(detail(e.detail))}">${esc(detail(e.detail)) || "—"}</td></tr>`));
}

async function adminRoles(c) {
  const r = await api("GET", "/api/admin/roles");
  if (loadFailed(c, [r], () => adminRoles(c))) return;
  const roles = (r.data && r.data.roles) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-role">New role</button></div>` +
    panelTable("Role policies", ["Role", "Allowed models", "Passthrough", "Limits", ""],
      roles.map((rp) => `<tr><td class="mono">${esc(rp.role)}</td>
        <td>${(rp.allowed_models || []).map(esc).join(", ")}</td>
        <td>${rp.allow_passthrough ? "yes" : "no"}</td>
        <td>${fmtLimits(rp.limits)}</td>
        <td class="actions"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(rp))}'>Edit</button></td></tr>`));
  $("#new-role").addEventListener("click", () => editRole(c, {}));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editRole(c, JSON.parse(b.getAttribute("data-edit")))));
}

function editRole(c, rp) {
  modalForm(rp.role ? `Edit role ${rp.role}` : "New role", [
    { name: "role", label: "Role name", value: rp.role || "", disabled: !!rp.role },
    { name: "allowed_models", label: "Allowed models (comma; * = all)", value: (rp.allowed_models || []).join(", ") },
    { name: "allow_passthrough", label: "Allow explicit provider passthrough", type: "checkbox", value: !!rp.allow_passthrough },
    { name: "limits", label: "Limits per rolling window (empty = no limit)", type: "limits", value: rp.limits || {} },
  ], async (v) => {
    const body = {
      allowed_models: v.allowed_models.split(",").map((s) => s.trim()).filter(Boolean),
      allow_passthrough: v.allow_passthrough,
      limits: v.limits,
    };
    const x = await api("PUT", `/api/admin/roles/${encodeURIComponent(v.role)}`, body);
    if (x.ok) { toast("Role saved"); adminRoles(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

async function adminProviders(c) {
  const r = await api("GET", "/api/admin/providers");
  if (loadFailed(c, [r], () => adminProviders(c))) return;
  const ps = (r.data && r.data.providers) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-prov">New provider</button></div>` +
    panelTable("Providers", ["Name", "Kind", "Base URL", "Key", "Concurrency", "Enabled", ""],
      ps.map((p) => `<tr><td class="mono">${esc(p.name)}</td><td>${esc(p.kind)}</td>
        <td class="mono">${esc(p.base_url) || "—"}</td>
        <td>${credentialBadge(p)}</td>
        <td>${p.max_concurrency > 0 ? p.max_concurrency : "∞"}</td>
        <td>${p.enabled ? "yes" : "no"}</td>
        <td class="actions"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(p))}'>Edit</button></td></tr>`));
  $("#new-prov").addEventListener("click", () => editProvider(c, {}));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editProvider(c, JSON.parse(b.getAttribute("data-edit")))));
}

// isGoogleCloud reports whether a provider record or a form's values describe
// a Google Cloud kind — Vertex AI, Google Speech or Google Text-to-Speech —
// addressed by structured configuration and authenticated by a refreshing
// token rather than a static key.
function isGoogleCloud(p) { return p.kind === "vertex" || p.kind === "google-speech" || p.kind === "google-tts"; }

// credentialBadge says what a provider authenticates with. A Google Cloud provider
// with nothing stored is not missing its key — it authenticates as the pod's
// own identity, which is the recommended configuration and must not read as
// broken in the list.
function credentialBadge(p) {
  if (p.has_credential) return `<span class="badge active">set</span>`;
  if (isGoogleCloud(p)) return `<span class="badge active">federated</span>`;
  return `<span class="badge neutral">none</span>`;
}

function editProvider(c, p) {
  const cfg = p.config || {};
  modalForm(p.name ? `Edit provider ${p.name}` : "New provider", [
    { name: "name", label: "Name", value: p.name || "", disabled: !!p.name },
    { name: "kind", label: "Kind", type: "select", options: ["mock", "openai", "openrouter", "xai", "groq", "ollama", "muse", "anthropic", "vertex", "google-speech", "google-tts"], value: p.kind || "mock" },
    { name: "base_url", label: "Base URL (optional override)", value: p.base_url || "" },
    // Google Cloud kinds are addressed by project and location rather than by
    // a URL, and hold an OAuth2 credential rather than a key — so their fields
    // appear only for those kinds, and the API-key field steps aside rather than
    // being repurposed for a credential it cannot carry.
    { name: "project", label: "Cloud project", value: cfg.project || "", showWhen: isGoogleCloud },
    { name: "location", label: "Location (blank = global)", value: cfg.location || "", showWhen: isGoogleCloud },
    // A blank credential keeps the stored one, so removing it is a separate,
    // explicit checkbox — offered only when there is something to remove, and
    // hiding the credential inputs while ticked, since the admin API rejects a
    // save that both sets and clears.
    { name: "clear_credential", type: "checkbox", value: false, showWhen: () => !!p.has_credential,
      label: "Remove the stored credential — vertex, google-speech and google-tts then authenticate as the pod's own identity; other kinds are left without a key" },
    { name: "api_key", label: p.has_credential ? "API key (set — blank keeps current)" : "API key", type: "password", value: "", placeholder: p.has_credential ? "•••••• stored" : "", showWhen: (v) => !isGoogleCloud(v) && !v.clear_credential },
    { name: "credential_json", type: "textarea", value: "", showWhen: (v) => isGoogleCloud(v) && !v.clear_credential,
      label: p.has_credential
        ? "Service-account JSON (stored — blank keeps it)"
        : "Service-account JSON — leave blank to authenticate as the pod's own identity" },
    { name: "max_concurrency", label: "Max concurrency (0 = unlimited)", type: "number", min: 0, value: p.max_concurrency ?? 0 },
    { name: "enabled", label: "Enabled", type: "checkbox", value: p.enabled !== false },
  ], async (v) => {
    const body = { kind: v.kind, base_url: v.base_url, enabled: v.enabled, max_concurrency: Number(v.max_concurrency) || 0 };
    if (isGoogleCloud(v)) {
      // Only the kinds that have structured configuration send it: the admin
      // API keeps the stored configuration when a save omits it, so an empty
      // object from another kind would erase a project as a side effect.
      body.config = { project: v.project.trim(), location: v.location.trim() };
    }
    if (v.clear_credential) body.clear_credential = true;
    else if (isGoogleCloud(v)) body.credential_json = v.credential_json.trim();
    else body.api_key = v.api_key;
    const x = await api("PUT", `/api/admin/providers/${encodeURIComponent(v.name)}`, body);
    if (x.ok) { toast("Provider saved"); adminProviders(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

async function adminPricing(c) {
  const r = await api("GET", "/api/admin/pricing");
  const pr = await api("GET", "/api/admin/providers");
  if (loadFailed(c, [r, pr], () => adminPricing(c))) return;
  const ps = (r.data && r.data.pricing) || [];
  const providers = ((pr.data && pr.data.providers) || []).map((p) => p.name);
  c.innerHTML = `<div class="row" style="margin-bottom:1rem;gap:.5rem">
      <button class="btn sm" id="new-price">New price</button>
      <select id="import-provider">${providers.map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join("")}</select>
      <button class="btn ghost sm" id="import-prices">Import prices</button>
    </div>` +
    panelTable("Pricing", ["Provider", "Model", "Unit", "Input $/1M", "Output $/1M", "Long-prompt tier ($/1M)", ""],
      ps.map((p) => `<tr><td class="mono">${esc(p.provider) || "(any)"}</td><td class="mono">${esc(p.model)}</td>
        <td>${esc(p.unit || "tokens")}</td><td>${fmtUSD(p.input_per_1m)}</td><td>${fmtUSD(p.output_per_1m)}</td>
        <td class="mono">${esc(contextTierLabel(p))}</td>
        <td class="actions"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(p))}'>Edit</button></td></tr>`));
  $("#new-price").addEventListener("click", () => editPrice(c, {}));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editPrice(c, JSON.parse(b.getAttribute("data-edit")))));
  $("#import-prices").addEventListener("click", async () => {
    const name = $("#import-provider").value;
    // Empty on a fresh install before any provider is added — the <select>
    // has no <option>s at all, so a silent return here looked like the
    // button did nothing.
    if (!name) { toast("No providers configured yet", "err"); return; }
    const x = await api("POST", `/api/admin/pricing/import/${encodeURIComponent(name)}`);
    if (x.ok) {
      const n = (x.data && x.data.imported) || 0;
      if ((x.data && x.data.unsupported) || n === 0) toast(`${name}'s catalog publishes no prices`);
      else toast(`Imported ${n} prices from ${name}`);
      adminPricing(c);
    } else toast((x.data && x.data.error) || "Failed", "err");
  });
}

// A vendor that charges more above a context threshold (Gemini 2.5 Pro doubles
// above 200k prompt tokens) is one row with a breakpoint on it, so the list has
// to say so — otherwise the row's visible rates are half the story.
function contextTierLabel(p) {
  const t = Number(p.context_threshold) || 0;
  if (t <= 0) return "—";
  return `> ${t.toLocaleString("en-US")} tok → ${fmtUSD(p.input_per_1m_above)} / ${fmtUSD(p.output_per_1m_above)}`;
}

async function editPrice(c, p) {
  const pr = await api("GET", "/api/admin/providers");
  if (!pr.ok) { toast("Could not load providers: " + apiError(pr), "err"); return; }
  const providers = ((pr.data && pr.data.providers) || []).map((x) => x.name);
  const providerOptions = [{ value: "", label: "(any)" }, ...providers.map((n) => ({ value: n, label: n }))];
  // The two above-threshold rates are only meaningful on a token row that has a
  // breakpoint, so they appear once one is entered rather than sitting empty on
  // every audio row.
  const tiered = (v) => v.unit === "tokens" && (Number(v.context_threshold) || 0) > 0;
  modalForm(p.model ? `Edit price ${p.model}` : "New price", [
    { name: "model", label: "Upstream model", value: p.model || "", disabled: !!p.model },
    { name: "provider", label: "Provider", type: "select", options: providerOptions, value: p.provider || "", disabled: !!p.model },
    { name: "unit", label: "Unit ($ / 1M of this)", type: "select",
      options: ["tokens", "audio_second", "text_char"], value: p.unit || "tokens" },
    { name: "input_per_1m", label: "Input $ / 1M", type: "number", min: 0, step: "any", value: p.input_per_1m ?? 0 },
    { name: "output_per_1m", label: "Output $ / 1M", type: "number", min: 0, step: "any", value: p.output_per_1m ?? 0 },
    { name: "context_threshold", label: "Long-prompt threshold (prompt tokens, 0 = none)",
      type: "number", min: 0, value: p.context_threshold ?? 0, showWhen: (v) => v.unit === "tokens" },
    { name: "input_per_1m_above", label: "Input $ / 1M above the threshold",
      type: "number", min: 0, step: "any", value: p.input_per_1m_above ?? 0, showWhen: tiered },
    { name: "output_per_1m_above", label: "Output $ / 1M above the threshold",
      type: "number", min: 0, step: "any", value: p.output_per_1m_above ?? 0, showWhen: tiered },
  ], async (v) => {
    // Sent only for the row shape that reads them, so clearing the threshold
    // (or switching to an audio unit) does not leave behind rates nothing will
    // ever look at. Same predicate the fields are shown by, so what was
    // visible is exactly what is saved.
    const tier = tiered(v);
    const x = await api("PUT", `/api/admin/pricing/${encodeURIComponent(v.model)}`, {
      provider: v.provider, unit: v.unit,
      input_per_1m: Number(v.input_per_1m), output_per_1m: Number(v.output_per_1m),
      context_threshold: tier ? Number(v.context_threshold) : 0,
      input_per_1m_above: tier ? Number(v.input_per_1m_above) : 0,
      output_per_1m_above: tier ? Number(v.output_per_1m_above) : 0,
    });
    if (x.ok) { toast("Pricing saved"); adminPricing(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

// targetOptionsText renders a target's free-form options object for the
// alias editor: empty when there is nothing set, so the placeholder shows.
// Top-level so web/targetoptions.test.mjs can extract it.
function targetOptionsText(o) {
  return o && typeof o === "object" && Object.keys(o).length ? JSON.stringify(o) : "";
}

// parseTargetOptions reads the alias editor's options field back: empty is
// the empty object, anything else must be a JSON object. The server checks
// the keys it knows; this only catches a typo before the round trip.
function parseTargetOptions(text) {
  const t = (text || "").trim();
  if (!t) return {};
  let v;
  try { v = JSON.parse(t); } catch (e) { throw new Error("options must be valid JSON"); }
  if (v === null || typeof v !== "object" || Array.isArray(v)) throw new Error("options must be a JSON object");
  return v;
}

// msToSecondsText shows a stored millisecond duration as seconds for an
// editor field: unset is empty, 0 stays "0", 1500 is "1.5". Top-level so
// web/targetoptions.test.mjs can extract it.
function msToSecondsText(ms) {
  if (typeof ms !== "number" || !Number.isFinite(ms)) return "";
  return String(Math.round(ms) / 1000);
}

// parseSecondsToMs reads a seconds field back as whole milliseconds: empty is
// undefined (not set), otherwise a non-negative number, or a positive one when
// positive is true. Decimals are fine down to one millisecond.
function parseSecondsToMs(text, what, positive) {
  const t = String(text ?? "").trim();
  if (!t) return undefined;
  const s = Number(t);
  const rule = positive ? "a number of seconds above 0" : "a number of seconds, 0 or more";
  if (!Number.isFinite(s) || s < 0 || (positive && s === 0)) throw new Error(`${what} must be ${rule}`);
  const ms = Math.round(s * 1000);
  if (s > 0 && ms === 0) throw new Error(`${what} must be at least 0.001 seconds`);
  return ms;
}

// targetOptionsForm splits a target's stored options into the alias editor's
// structured fields (strings, "" = not set) and the Advanced JSON text, which
// holds every other key, including breaker keys without a field of their
// own. A key is lifted into a field only when it has the type that field
// writes back, so a round trip never changes it. Top-level so
// web/targetoptions.test.mjs can extract it.
function targetOptionsForm(o) {
  const rest = o && typeof o === "object" && !Array.isArray(o) ? { ...o } : {};
  const tri = (v) => (v === true ? "yes" : "no");
  const f = { timeout: "", fallback_on_auth: "", thinking: "", breaker_enabled: "", breaker_failures: "", breaker_cooldown: "" };
  if (typeof rest.timeout_ms === "number") { f.timeout = msToSecondsText(rest.timeout_ms); delete rest.timeout_ms; }
  if (typeof rest.fallback_on_auth === "boolean") { f.fallback_on_auth = tri(rest.fallback_on_auth); delete rest.fallback_on_auth; }
  if (rest.thinking === "off") { f.thinking = "off"; delete rest.thinking; }
  const b = rest.breaker;
  if (b && typeof b === "object" && !Array.isArray(b)) {
    const br = { ...b };
    if (typeof br.enabled === "boolean") { f.breaker_enabled = tri(br.enabled); delete br.enabled; }
    if (Number.isInteger(br.failures)) { f.breaker_failures = String(br.failures); delete br.failures; }
    if (typeof br.cooldown_ms === "number") { f.breaker_cooldown = msToSecondsText(br.cooldown_ms); delete br.cooldown_ms; }
    if (Object.keys(br).length) rest.breaker = br; else delete rest.breaker;
  }
  f.advanced = targetOptionsText(rest);
  return f;
}

// targetOptionsFromForm is targetOptionsForm in reverse: the Advanced JSON is
// the base, every structured field that is set overwrites its key, and an
// unset field leaves the key out (or as Advanced has it). Throws with the
// field's name on bad input.
function targetOptionsFromForm(f) {
  let o;
  try { o = parseTargetOptions(f.advanced); } catch (e) { throw new Error("Advanced (JSON): " + e.message); }
  const tri = (v) => (v === "yes" ? true : v === "no" ? false : undefined);
  const timeout = parseSecondsToMs(f.timeout, "timeout");
  if (timeout !== undefined) o.timeout_ms = timeout;
  const auth = tri(f.fallback_on_auth);
  if (auth !== undefined) o.fallback_on_auth = auth;
  if (f.thinking) o.thinking = f.thinking;
  const br = {};
  const enabled = tri(f.breaker_enabled);
  if (enabled !== undefined) br.enabled = enabled;
  const failures = String(f.breaker_failures ?? "").trim();
  if (failures) {
    if (!/^\d+$/.test(failures) || Number(failures) < 1) throw new Error("breaker failures must be a whole number above 0");
    br.failures = Number(failures);
  }
  const cooldown = parseSecondsToMs(f.breaker_cooldown, "breaker cooldown", true);
  if (cooldown !== undefined) br.cooldown_ms = cooldown;
  if (Object.keys(br).length) {
    if (o.breaker !== undefined && (o.breaker === null || typeof o.breaker !== "object" || Array.isArray(o.breaker))) {
      throw new Error("Advanced (JSON): breaker must be a JSON object");
    }
    o.breaker = { ...(o.breaker || {}), ...br };
  }
  return o;
}

// affinityTTLText shows an alias's stored session pin TTL (seconds) as hours
// for the editor; the default (0) is an empty field. Top-level so
// web/affinityttl.test.mjs can extract it.
function affinityTTLText(seconds) {
  const s = Number(seconds) || 0;
  return s > 0 ? String(Math.round((s / 3600) * 100) / 100) : "";
}

// parseAffinityTTL turns the editor's hours into the seconds the API stores:
// empty is 0 (the gateway default), anything else must lie in (0, 168].
function parseAffinityTTL(text) {
  const t = (text || "").trim();
  if (!t) return 0;
  const h = Number(t);
  if (!Number.isFinite(h) || h <= 0 || h > 168) throw new Error("session pin TTL must be a number of hours between 0 and 168");
  return Math.round(h * 3600);
}

// tierHealthRows renders the breaker of each tier from the alias health
// endpoint as table rows. An open or probing tier gets a Release button.
// Top-level so web/tierhealth.test.mjs can extract it.
function tierHealthRows(tiers) {
  const badge = { closed: "active", open: "revoked", half_open: "admin" };
  const fmt = (s) => (s ? new Date(s).toLocaleString() : "—");
  const targetText = (x) => {
    let s = `${esc(x.provider)}/${esc(x.upstream_model)}`;
    if (x.price_usd_per_1m != null) s += ` ($${Number(x.price_usd_per_1m).toFixed(2)}/1M)`;
    if (x.availability != null) s += ` (${Math.round(Number(x.availability) * 100)}% avail)`;
    if (x.unavailable) s += ` [unavailable ${Math.round(Number(x.unavailable_remaining_ms) / 1000)}s]`;
    return s;
  };
  return (tiers || []).map((t) => {
    const state = t.breaker_enabled ? t.state : "off";
    const quarantined = t.state === "open" || t.state === "half_open";
    return `<tr><td class="mono">p${Number(t.tier)}</td>
      <td class="mono">${(t.targets || []).map(targetText).join(", ") || "—"}</td>
      <td><span class="badge ${badge[state] || "neutral"}">${esc(state === "half_open" ? "probing" : state)}</span></td>
      <td>${esc(quarantined ? t.reason || "" : "") || "—"}</td>
      <td>${quarantined ? esc(fmt(t.open_until)) : "—"}</td>
      <td>${t.cooldown_ms ? `${Math.round(Number(t.cooldown_ms) / 1000)}s` : "—"}</td>
      <td style="text-align:right">${quarantined ? `<button class="btn ghost sm" data-release="${Number(t.tier)}">Release</button>` : ""}</td></tr>`;
  });
}

async function showTierHealth(alias) {
  const r = await api("GET", `/api/admin/aliases/${encodeURIComponent(alias)}/health`);
  if (!r.ok) { toast("Failed to load tier health", "err"); return; }
  const bg = document.createElement("div");
  bg.className = "modal-bg";
  bg.innerHTML = `<div class="modal modal-wide" style="max-width:820px">
    ${panelTable("Tier health — " + alias, ["Tier", "Targets", "Breaker", "Reason", "Open until", "Cooldown", ""], tierHealthRows(r.data.tiers))}
    <div class="row modal-foot">
      <button type="button" class="btn ghost" id="th-refresh">Refresh</button>
      <button type="button" class="btn" id="th-close">Close</button></div></div>`;
  document.body.appendChild(bg);
  const close = wireModalDismiss(bg);
  $("#th-close", bg).addEventListener("click", close);
  $("#th-refresh", bg).addEventListener("click", () => { close(); showTierHealth(alias); });
  bg.querySelectorAll("[data-release]").forEach((b) => b.addEventListener("click", async () => {
    const tier = b.getAttribute("data-release");
    if (!confirm(`Release tier p${tier} of ${alias}? Requests will reach it again at once.`)) return;
    const x = await api("POST", `/api/admin/aliases/${encodeURIComponent(alias)}/tiers/${tier}/release`);
    if (x.ok) { toast("Released"); close(); showTierHealth(alias); } else toast("Release failed", "err");
  }));
}

// synthCacheTTLText shows an alias's stored synthesis cache TTL (seconds) as
// days for the editor; the default (0) is an empty field. Top-level so
// web/synthcachettl.test.mjs can extract it.
function synthCacheTTLText(seconds) {
  const s = Number(seconds) || 0;
  return s > 0 ? String(Math.round((s / 86400) * 100) / 100) : "";
}

// parseSynthCacheTTL turns the editor's days into the seconds the API
// stores: empty is 0 (the gateway default), anything else must lie in
// (0, 30].
function parseSynthCacheTTL(text) {
  const t = (text || "").trim();
  if (!t) return 0;
  const d = Number(t);
  if (!Number.isFinite(d) || d <= 0 || d > 30) throw new Error("synthesis cache TTL must be a number of days between 0 and 30");
  return Math.round(d * 86400);
}

async function adminAliases(c) {
  const r = await api("GET", "/api/admin/aliases");
  if (loadFailed(c, [r], () => adminAliases(c))) return;
  const al = (r.data && r.data.aliases) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-alias">New alias</button></div>` +
    panelTable("Model aliases", ["Alias", "Protocol", "Strategy", "BERT", "Targets", ""],
      al.map((a) => `<tr><td class="mono">${esc(a.alias)}</td><td>${esc(a.protocol)}</td>
        <td>${esc(a.strategy || "round_robin")}</td>
        <td>${a.dlp_model_scan ? `<span class="badge neutral">on</span>` : `<span class="badge revoked">off</span>`}</td>
        <td class="mono">${(a.targets || []).map((t) => `${esc(t.provider)}/${esc(t.upstream_model)} (p${t.priority}${t.options && typeof t.options.timeout_ms === "number" ? (t.options.timeout_ms ? `, ${msToSecondsText(t.options.timeout_ms)}s` : ", no timeout") : ""})`).join("<br>") || "—"}</td>
        <td class="actions"><button class="btn ghost sm" data-health="${esc(a.alias)}">Health</button>
          <button class="btn ghost sm" data-edit='${esc(JSON.stringify(a))}'>Edit</button>
          <button class="btn danger sm" data-del="${esc(a.alias)}">Delete</button></td></tr>`)) +
    `<div id="failover-panel"></div>`;
  $("#new-alias").addEventListener("click", () => editAlias(c, {}));
  document.querySelectorAll("[data-health]").forEach((b) =>
    b.addEventListener("click", () => showTierHealth(b.getAttribute("data-health"))));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editAlias(c, JSON.parse(b.getAttribute("data-edit")))));
  document.querySelectorAll("[data-del]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm("Delete alias " + b.getAttribute("data-del") + "?")) return;
    const x = await api("DELETE", `/api/admin/aliases/${encodeURIComponent(b.getAttribute("data-del"))}`);
    if (x.ok) { toast("Deleted"); adminAliases(c); } else toast("Failed", "err");
  }));
  failoverPanel($("#failover-panel"));
}

// failoverPanel edits the gateway-wide failover defaults
// (GET/PUT /api/admin/failover) that every alias target falls back to for
// an option it does not set itself.
async function failoverPanel(el) {
  const r = await api("GET", "/api/admin/failover");
  if (!el.isConnected) return;
  if (loadFailed(el, [r], () => failoverPanel(el))) return;
  const f = r.data || {};
  el.innerHTML = `<div class="panel"><div class="panel-head"><h2>Failover defaults (all aliases)</h2></div>
    <div class="panel-body">
      <div class="hint">Used by every target whose own options do not set the key.</div>
      <div class="form-grid">
        <label class="field"><span class="lab">Time budget per target, seconds (0 = none)</span>
          <input id="fo-timeout" type="text" inputmode="decimal" value="${esc(msToSecondsText(Number(f.timeout_ms) || 0))}" /></label>
        <label class="field check"><input type="checkbox" id="fo-auth" ${f.fallback_on_auth ? "checked" : ""} /><span class="lab">Try the next tier on upstream auth/billing errors</span></label>
        <label class="field"><span class="lab">Unavailable backoff start, ms</span>
          <input id="fo-uinit" type="number" min="0" value="${Number(f.unavailable_initial_ms) || 0}" /></label>
        <label class="field"><span class="lab">Unavailable backoff ceiling, ms</span>
          <input id="fo-umax" type="number" min="0" value="${Number(f.unavailable_max_ms) || 0}" /></label>
        <label class="field span-2"><span class="lab">Circuit breaker defaults (JSON, same keys as a target's <span class="mono">breaker</span> option)</span>
          <input id="fo-breaker" class="mono" value="${esc(targetOptionsText(f.breaker))}" placeholder='e.g. {"enabled":true,"failures":3,"cooldown_ms":60000}' /></label>
      </div>
      <div class="panel-actions"><button class="btn" id="fo-save">Save failover defaults</button></div>
    </div></div>`;
  $("#fo-save", el).addEventListener("click", async () => {
    let breaker, timeoutMS;
    try { timeoutMS = parseSecondsToMs($("#fo-timeout", el).value, "Time budget") ?? 0; } catch (e) { toast(e.message, "err"); return; }
    try { breaker = parseTargetOptions($("#fo-breaker", el).value); } catch (e) { toast("breaker: " + e.message, "err"); return; }
    const x = await api("PUT", "/api/admin/failover", {
      timeout_ms: timeoutMS,
      fallback_on_auth: $("#fo-auth", el).checked,
      unavailable_initial_ms: Number($("#fo-uinit", el).value) || 0,
      unavailable_max_ms: Number($("#fo-umax", el).value) || 0,
      breaker,
    });
    if (x.ok) toast("Failover defaults saved"); else toast(apiError(x), "err");
  });
}

async function editAlias(c, a) {
  const pr = await api("GET", "/api/admin/providers");
  // Existing alias names, for the rename-collision guard on save.
  const ar = await api("GET", "/api/admin/aliases");
  if (!pr.ok || !ar.ok) { toast("Could not open the editor: " + apiError(!pr.ok ? pr : ar), "err"); return; }
  let providers = ((pr.data && pr.data.providers) || []).map((p) => p.name);
  if (providers.length === 0) providers = ["mock"];
  const existingAliases = ((ar.data && ar.data.aliases) || []).map((x) => x.alias);
  // Gateway-wide failover defaults, shown as what an unset target field
  // means. Optional: without them the fields just say "default".
  const fr = await api("GET", "/api/admin/failover");
  const fo = fr.ok && fr.data ? fr.data : null;
  const fob = fo && fo.breaker && typeof fo.breaker === "object" ? fo.breaker : {};
  const yesNo = (v) => (typeof v === "boolean" ? ` (${v ? "yes" : "no"})` : "");
  const dflt = {
    timeout: !fo ? "default" : Number(fo.timeout_ms) > 0 ? `default ${msToSecondsText(Number(fo.timeout_ms))}` : "default: none",
    auth: "default" + (fo ? yesNo(!!fo.fallback_on_auth) : ""),
    brEnabled: "default" + yesNo(fob.enabled),
    brFailures: Number.isInteger(fob.failures) ? `default ${fob.failures}` : "default",
    brCooldown: typeof fob.cooldown_ms === "number" ? `default ${msToSecondsText(fob.cooldown_ms)}` : "default",
  };

  const targets = (a.targets && a.targets.length)
    ? a.targets
    : [{ provider: providers[0], upstream_model: "" }];

  const bg = document.createElement("div");
  bg.className = "modal-bg";
  bg.innerHTML = `<div class="modal modal-wide">
    <h3>${esc(a.alias ? "Edit alias " + a.alias : "New alias")}</h3>
    <div class="form-grid">
      <label class="field span-2"><span class="lab">Alias (the model name clients request)</span>
        <input id="al-alias" value="${esc(a.alias || "")}" /></label>
      <label class="field"><span class="lab">Client protocol</span>
        <select id="al-proto">
          <option ${a.protocol !== "anthropic" ? "selected" : ""}>openai</option>
          <option ${a.protocol === "anthropic" ? "selected" : ""}>anthropic</option>
        </select></label>
      <label class="field"><span class="lab">Balancing within a priority tier</span>
        <select id="al-strategy">
          <option value="round_robin" ${a.strategy !== "least_busy" && a.strategy !== "lcr" ? "selected" : ""}>round_robin</option>
          <option value="least_busy" ${a.strategy === "least_busy" ? "selected" : ""}>least_busy</option>
          <option value="lcr" ${a.strategy === "lcr" ? "selected" : ""}>lcr (price / availability)</option>
        </select></label>
      <label class="field" data-al="lcr"><span class="lab">LCR mode</span>
        <select id="al-lcr-mode">
          <option value="price" ${(a.lcr_mode || "price") === "price" ? "selected" : ""}>price</option>
          <option value="availability" ${a.lcr_mode === "availability" ? "selected" : ""}>availability</option>
          <option value="both" ${a.lcr_mode === "both" ? "selected" : ""}>both (availability gate, then price)</option>
        </select></label>
      <label class="field" data-al="lcr-both"><span class="lab">LCR minimum availability (0..1)</span>
        <input id="al-lcr-minavail" type="number" min="0" max="1" step="0.01" value="${esc(String(a.lcr_min_availability ?? 0.5))}" /></label>
    </div>
    <div class="form-sect">Targets</div>
    <div class="hint">Same priority = load-balanced tier; higher number = fallback tier. Label is what the X-Backend-Model header shows — real provider/model names never leak.</div>
    <div class="tgt-head"><span>Tier</span><span>Provider</span><span>Upstream model</span><span>Label</span><span></span></div>
    <div id="al-targets"></div>
    <button type="button" class="btn ghost sm" id="al-add">+ Add target</button>
    <details class="hint opts-ref"><summary>Advanced (JSON) keys reference</summary>
      Timeout, auth/billing fallback, thinking and the breaker's on/failures/cooldown have their own fields; an empty field uses the failover default. Advanced holds the rest: <span class="mono">breaker</span> — further circuit breaker keys for the tier, e.g. <span class="mono">{"error_rate":0.5,"window_ms":60000,"min_requests":10,"max_cooldown_ms":600000,"stable_ms":300000}</span>; <span class="mono">recognition_models</span> — recognition model per BCP-47 language, e.g. <span class="mono">{"en-US":"telephony"}</span> (google-speech); <span class="mono">voices</span> — canonical voice → <span class="mono">{"voice","model"}</span> this target speaks it with; <span class="mono">default_voices</span> — voice per BCP-47 language for unmapped voices, e.g. <span class="mono">{"en":{"voice":"onyx"}}</span>; <span class="mono">send_language</span> — <span class="mono">true</span> sends an OpenAI-compatible speech target the request language (multilingual models with one voice); <span class="mono">stream_speech</span> — <span class="mono">true</span> streams this speech target's audio as it arrives.
    </details>
    <div class="form-sect">Behaviour</div>
    <div class="form-grid">
      <label class="field check"><input id="al-bert" type="checkbox" ${a.dlp_model_scan === false ? "" : "checked"} /><span class="lab">Layer-2 BERT scan (fuzzy PII)</span></label>
      <label class="field check"><input id="al-dlpaudio" type="checkbox" ${a.dlp_audio_scan === false ? "" : "checked"} /><span class="lab">DLP scan on audio transcripts/input (STT/TTS)</span></label>
      <label class="field check span-2"><input id="al-expose" type="checkbox" ${a.expose_backend_headers ? "checked" : ""} /><span class="lab">Expose which target answered (X-Backend-Model header)</span></label>
      <label class="field check span-2"><input id="al-affinity" type="checkbox" ${a.session_affinity ? "checked" : ""} /><span class="lab">Session affinity: a session (X-Session-Id) served by a fallback tier stays on it</span></label>
      <label class="field" data-al="affinity"><span class="lab">Session pin TTL, hours (empty = 4)</span>
        <input id="al-affinity-ttl" type="number" min="0" max="168" step="any" value="${esc(affinityTTLText(a.session_affinity_ttl_s))}" /></label>
      <label class="field check span-2"><input id="al-ttscache" type="checkbox" ${a.synthesis_cache ? "checked" : ""} /><span class="lab">Synthesis cache: answer a repeated speech request with the clip the serving tier rendered</span></label>
      <label class="field" data-al="ttscache"><span class="lab">Synthesis cache TTL, days (empty = 7)</span>
        <input id="al-ttscache-ttl" type="number" min="0" max="30" step="any" value="${esc(synthCacheTTLText(a.synthesis_cache_ttl_s))}" /></label>
    </div>
    <div class="row modal-foot">
      <button type="button" class="btn ghost" id="al-cancel">Cancel</button>
      <button type="button" class="btn" id="al-save">Save</button>
    </div>
  </div>`;
  document.body.appendChild(bg);
  const close = wireModalDismiss(bg);
  const tdiv = $("#al-targets", bg);

  // Fields that only apply under another setting stay hidden until it is
  // on. Hidden inputs keep their values and are saved as before.
  const applyVisibility = () => {
    const lcr = $("#al-strategy", bg).value === "lcr";
    $('[data-al="lcr"]', bg).hidden = !lcr;
    $('[data-al="lcr-both"]', bg).hidden = !(lcr && $("#al-lcr-mode", bg).value === "both");
    $('[data-al="affinity"]', bg).hidden = !$("#al-affinity", bg).checked;
    $('[data-al="ttscache"]', bg).hidden = !$("#al-ttscache", bg).checked;
  };
  applyVisibility();
  bg.addEventListener("change", applyVisibility);

  const provOpts = (sel) => providers.map((p) =>
    `<option value="${esc(p)}" ${p === sel ? "selected" : ""}>${esc(p)}</option>`).join("");
  // Upstream model suggestions: fetched per provider, memoized (as an
  // in-flight promise, so concurrent requests for the same provider share
  // one fetch) for the lifetime of this modal. Failure or unsupported ->
  // empty list; the model control falls back to free text either way.
  const modelLists = {};
  function fetchModels(prov) {
    if (!(prov in modelLists)) {
      modelLists[prov] = api("GET", `/api/admin/providers/${encodeURIComponent(prov)}/models`)
        .then((r) => (r.ok && r.data && r.data.models) || [])
        .catch(() => []);
    }
    return modelLists[prov];
  }
  // renderModelControl fills slot with a real <select class="t-model"> when
  // the provider's catalog is non-empty, or a free-text <input class="t-model">
  // otherwise — a datalist-based free-text input looks the same on desktop
  // but its suggestion dropdown is unreliable on mobile Safari, effectively
  // making model selection impossible on a phone. Reading .t-model's .value
  // works identically for either element, so the save path below never
  // needs to know which one is present. onModelChange fires once right
  // after the control is (re)built (covering a <select>'s auto-picked
  // first option, and a free-text input's starting value) and again on
  // every later change — used to keep the label field's default in sync.
  async function renderModelControl(slot, provSel, current, onModelChange) {
    const prov = provSel.value;
    const models = await fetchModels(prov);
    // Stale guard: the user may have switched provider again while this
    // fetch was in flight — never overwrite a newer selection's control.
    if (provSel.value !== prov) return;
    if (models.length === 0) {
      slot.innerHTML = `<input class="t-model" placeholder="upstream model" value="${esc(current)}" />`;
    } else {
      // Keep `current` selectable even if the catalog doesn't list it — an
      // existing alias may point at a model since removed from the catalog.
      const opts = current && !models.includes(current) ? [current, ...models] : models;
      slot.innerHTML = `<select class="t-model">${opts.map((m) =>
        `<option value="${esc(m)}" ${m === current ? "selected" : ""}>${esc(m)}</option>`).join("")}</select>`;
    }
    const modelEl = slot.querySelector(".t-model");
    modelEl.addEventListener(modelEl.tagName === "SELECT" ? "change" : "input", onModelChange);
    onModelChange();
  }
  const triOpts = (v, dl) => [["", dl], ["yes", "yes"], ["no", "no"]].map(([val, text]) =>
    `<option value="${val}" ${val === v ? "selected" : ""}>${esc(text)}</option>`).join("");
  function addRow(t) {
    const f = targetOptionsForm(t.options);
    const row = document.createElement("div");
    row.className = "tgt";
    row.innerHTML = `
      <input class="t-prio" type="number" min="0" value="${Number(t.priority) || 0}" title="priority / tier (same number = load-balanced)" aria-label="tier" />
      <select class="t-prov" aria-label="provider">${provOpts(t.provider)}</select>
      <span class="t-model-slot"></span>
      <input class="t-label" placeholder="label (X-Backend-Model)" value="${esc(t.display_label || "")}" aria-label="label" />
      <button type="button" class="btn danger sm t-del" title="remove target">×</button>
      <div class="t-opts">
        <label class="t-fld"><span class="t-lab">Timeout, s</span>
          <input class="t-timeout" type="text" inputmode="decimal" placeholder="${esc(dflt.timeout)}" value="${esc(f.timeout)}" title="time budget for this target in seconds; 0 = no budget; empty = failover default" /></label>
        <label class="t-fld t-auth-fld"><span class="t-lab" title="Fall back on upstream auth/billing errors">Auth/billing fallback</span>
          <select class="t-auth" title="Fall back on upstream auth/billing errors">${triOpts(f.fallback_on_auth, dflt.auth)}</select></label>
        <button type="button" class="btn ghost sm t-more-btn" aria-expanded="false"></button>
      </div>
      <div class="t-more" hidden>
        <label class="t-fld"><span class="t-lab">Thinking</span>
          <select class="t-thinking"><option value="">default (as the client asks)</option><option value="off" ${f.thinking === "off" ? "selected" : ""}>off</option></select></label>
        <div class="t-breaker"><span class="t-lab t-sub" title="The breaker is per tier: each key comes from the first target of the tier that sets it.">Breaker (tier)</span>
          <label class="t-fld"><span class="t-lab">On</span><select class="t-br-enabled">${triOpts(f.breaker_enabled, dflt.brEnabled)}</select></label>
          <label class="t-fld"><span class="t-lab">Failures</span><input class="t-br-failures" type="text" inputmode="numeric" placeholder="${esc(dflt.brFailures)}" value="${esc(f.breaker_failures)}" /></label>
          <label class="t-fld"><span class="t-lab">Cooldown, s</span><input class="t-br-cooldown" type="text" inputmode="decimal" placeholder="${esc(dflt.brCooldown)}" value="${esc(f.breaker_cooldown)}" /></label></div>
        <label class="t-fld t-adv"><span class="t-lab">Advanced (JSON)</span>
          <textarea class="t-advanced mono" rows="2" spellcheck="false" placeholder='e.g. {"voices":{"en-US-Neural2-F":{"voice":"nova"}}}'>${esc(f.advanced)}</textarea></label>
      </div>`;
    row.querySelector(".t-del").addEventListener("click", () => row.remove());
    // "More" says what is set behind it, so a collapsed row never hides a
    // setting from the operator.
    const moreBtn = row.querySelector(".t-more-btn");
    const morePanel = row.querySelector(".t-more");
    const markMore = () => {
      const set = [];
      if (row.querySelector(".t-thinking").value) set.push("thinking");
      if ([".t-br-enabled", ".t-br-failures", ".t-br-cooldown"].some((q) => row.querySelector(q).value.trim())) set.push("breaker");
      try {
        for (const k of Object.keys(parseTargetOptions(row.querySelector(".t-advanced").value))) if (!set.includes(k)) set.push(k);
      } catch (_) { set.push("advanced"); }
      moreBtn.textContent = (morePanel.hidden ? "More" : "Less") +
        (set.length ? ` · ${set[0]}${set.length > 1 ? ` +${set.length - 1}` : ""}` : "");
      moreBtn.title = set.length ? "Set: " + set.join(", ") : "";
      // The Advanced field grows with its content while the panel is open.
      const adv = row.querySelector(".t-advanced");
      if (!morePanel.hidden) { adv.style.height = "auto"; adv.style.height = Math.min(adv.scrollHeight + 2, 220) + "px"; }
    };
    row.openMore = () => { morePanel.hidden = false; moreBtn.setAttribute("aria-expanded", "true"); markMore(); };
    moreBtn.addEventListener("click", () => {
      morePanel.hidden = !morePanel.hidden;
      moreBtn.setAttribute("aria-expanded", String(!morePanel.hidden));
      markMore();
    });
    morePanel.addEventListener("input", markMore);
    morePanel.addEventListener("change", markMore);
    markMore();
    const slot = row.querySelector(".t-model-slot");
    const provSel = row.querySelector(".t-prov");
    const labelInput = row.querySelector(".t-label");

    // Default the label to "provider:model" (matching the convention already
    // used across existing aliases, e.g. "groq:qwen3.6-27b") so an operator
    // adding a target doesn't have to type one — but stop touching it the
    // moment they type their own, so a later provider/model change never
    // clobbers a deliberate custom label.
    let autoLabel = !labelInput.value;
    labelInput.addEventListener("input", () => { autoLabel = false; });
    function maybeUpdateLabel() {
      if (!autoLabel) return;
      const model = row.querySelector(".t-model").value;
      if (model) labelInput.value = `${provSel.value}:${model}`;
    }

    renderModelControl(slot, provSel, t.upstream_model || "", maybeUpdateLabel);
    provSel.addEventListener("change", () => {
      // Switching provider invalidates the previous model selection —
      // models are provider-specific — so the new control always starts
      // with no prior value.
      renderModelControl(slot, provSel, "", maybeUpdateLabel);
    });
    tdiv.appendChild(row);
  }
  targets.forEach(addRow);
  $("#al-add", bg).addEventListener("click", () =>
    addRow({ provider: providers[0], upstream_model: "" }));
  $("#al-cancel", bg).addEventListener("click", close);
  $("#al-save", bg).addEventListener("click", async () => {
    const alias = $("#al-alias", bg).value.trim();
    if (!alias) { toast("Alias is required", "err"); return; }
    // PUT is an upsert: renaming (or creating) onto another existing alias
    // would silently overwrite it. Block the collision instead.
    if (alias !== a.alias && existingAliases.includes(alias)) {
      toast(`Alias ${alias} already exists`, "err"); return;
    }
    let tlist, affinityTTL, cacheTTL;
    try {
      affinityTTL = parseAffinityTTL($("#al-affinity-ttl", bg).value);
      cacheTTL = parseSynthCacheTTL($("#al-ttscache-ttl", bg).value);
      tlist = [...tdiv.querySelectorAll(".tgt")].map((r, i) => {
        const v = (q) => r.querySelector(q).value;
        let options;
        try {
          options = targetOptionsFromForm({
            timeout: v(".t-timeout"), fallback_on_auth: v(".t-auth"), thinking: v(".t-thinking"),
            breaker_enabled: v(".t-br-enabled"), breaker_failures: v(".t-br-failures"),
            breaker_cooldown: v(".t-br-cooldown"), advanced: v(".t-advanced"),
          });
        } catch (e) {
          // The bad field may sit behind "More": open it so it can be seen.
          if (!/^timeout/.test(e.message)) r.openMore();
          throw new Error(`Target ${i + 1}: ${e.message}`);
        }
        return {
          priority: Number(r.querySelector(".t-prio").value) || 0,
          provider: r.querySelector(".t-prov").value,
          upstream_model: r.querySelector(".t-model").value.trim(),
          display_label: r.querySelector(".t-label").value.trim(),
          options,
        };
      }).filter((t) => t.upstream_model);
    } catch (e) { toast(e.message, "err"); return; }
    if (tlist.length === 0) { toast("Add at least one target with a model", "err"); return; }
    const x = await api("PUT", `/api/admin/aliases/${encodeURIComponent(alias)}`,
      { protocol: $("#al-proto", bg).value, strategy: $("#al-strategy", bg).value, targets: tlist,
        lcr_mode: $("#al-lcr-mode", bg).value, lcr_min_availability: Number($("#al-lcr-minavail", bg).value),
        dlp_model_scan: $("#al-bert", bg).checked, expose_backend_headers: $("#al-expose", bg).checked, dlp_audio_scan: $("#al-dlpaudio", bg).checked,
        session_affinity: $("#al-affinity", bg).checked, session_affinity_ttl_s: affinityTTL,
        synthesis_cache: $("#al-ttscache", bg).checked, synthesis_cache_ttl_s: cacheTTL });
    if (!x.ok) { toast((x.data && x.data.error) || "Failed", "err"); return; }
    // Rename = save under the new name, then drop the old one. Role
    // policies and pricing that reference the old name are NOT rewritten.
    if (a.alias && a.alias !== alias) {
      const d = await api("DELETE", `/api/admin/aliases/${encodeURIComponent(a.alias)}`);
      if (!d.ok) { toast(`Saved as ${alias}, but deleting old ${a.alias} failed`, "err"); close(); adminAliases(c); return; }
      toast(`Renamed to ${alias} — update role policies that referenced ${a.alias}`);
    } else {
      toast("Alias saved");
    }
    close(); adminAliases(c);
  });
}

async function adminDLP(c) {
  const [cfgR, incR, whR, capR, patR, spR] = await Promise.all([
    api("GET", "/api/admin/dlp"),
    api("GET", "/api/admin/dlp/incidents"),
    api("GET", "/api/admin/webhooks"),
    api("GET", "/api/admin/capture"),
    api("GET", "/api/admin/dlp/patterns"),
    api("GET", "/api/admin/secondpass"),
  ]);
  // Every editor below is built only from a successful GET. A failed load
  // shows an error box with Retry in place of the form, so there is no Save
  // button that could write defaults over the stored policy.
  const failBox = (title, rs) => {
    const bad = rs.find((r) => !r.ok);
    return `<div class="panel load-error"><div class="panel-head"><h2>${esc(title)}</h2></div>
      <div class="load-error-body"><p class="err-text">Could not load: ${esc(apiError(bad))}</p>
      <button class="btn sm" type="button" data-retry>Retry</button></div></div>`;
  };
  const policyOK = cfgR.ok && patR.ok;
  const d = cfgR.data || {};
  const cap = capR.data || {};
  const sp = spR.data || {};
  const actBadge = (a) => a === "blocked" ? "revoked" : (a === "redacted" ? "admin" : "neutral");

  // Sensitive Info Detection (guardrails): build grouped pattern toggles.
  const catalog = (patR.data && patR.data.patterns) || [];
  const curPat = d.patterns || {};
  const patOn = (p) => (p.label in curPat) ? curPat[p.label] : p.default_on;
  const togRow = (p) => `<label class="check">
      <input type="checkbox" class="pat-tog" data-label="${esc(p.label)}" ${patOn(p) ? "checked" : ""} />
      <span class="mono">${esc(p.label)}${p.model ? ` <span class="muted">· adds latency</span>` : ""}</span></label>`;
  const group = (title, items) => items.length ? `<div class="form-sect">${title}</div><div class="pat-grid">${items.map(togRow).join("")}</div>` : "";
  const patGroups =
    group("Secrets", catalog.filter((p) => p.category === "secret")) +
    group("PII", catalog.filter((p) => p.category === "pii" && !p.model)) +
    group("Model (BERT sidecar)", catalog.filter((p) => p.model));
  const customRow = (cp) => `<div class="custom-row">
      <input class="cp-label" placeholder="label" value="${esc(cp.label || "")}" />
      <input class="cp-regex mono" placeholder="regex" value="${esc(cp.regex || "")}" />
      <label class="check"><input type="checkbox" class="cp-en" ${cp.enabled ? "checked" : ""} />on</label>
      <button class="btn danger sm cp-del" type="button" title="remove">×</button></div>`;
  const customRows = ((d.custom_patterns) || []).map(customRow).join("");

  const policyPanel = !policyOK ? failBox("DLP policy & detection", [cfgR, patR]) : `
    <div class="panel"><div class="panel-head"><h2>DLP policy &amp; detection</h2>
      <button class="btn ghost sm" id="pat-all" type="button">Enable all patterns</button></div>
      <div class="panel-body">
        <div class="form-grid">
          <label class="field check"><input type="checkbox" id="dlp-en" ${d.enabled ? "checked" : ""} /><span class="lab">Scan requests for secrets / tokens</span></label>
          <label class="field"><span class="lab">Action on detection</span>
            <select id="dlp-act">${["off", "flag", "redact", "block"].map((a) =>
              `<option ${a === d.action ? "selected" : ""}>${a}</option>`).join("")}</select></label>
        </div>
        <div class="form-sect">BERT NER sidecar (layer 2 — fuzzy/contextual PII)</div>
        <div class="form-grid">
          <label class="field check"><input type="checkbox" id="dlp-men" ${d.model_enabled ? "checked" : ""} /><span class="lab">Use model sidecar</span></label>
          <label class="field"><span class="lab">Sidecar URL</span>
            <input id="dlp-murl" value="${esc(d.model_url || "http://dlp-bert:8000")}" /></label>
          <label class="field"><span class="lab">Min score (0–1)</span>
            <input id="dlp-mscore" type="number" step="0.05" min="0" max="1" value="${d.model_min_score ?? 0.5}" /></label>
          <label class="field"><span class="lab">Max concurrent scans per endpoint (0 = unlimited)</span>
            <input id="dlp-mconc" type="number" min="0" value="${d.model_max_concurrency ?? 0}" /></label>
          <label class="field"><span class="lab">Model scan scope</span>
            <select id="dlp-mscope">
              <option value="last_user" ${d.model_scan_scope !== "all" ? "selected" : ""}>last user message (default)</option>
              <option value="all" ${d.model_scan_scope === "all" ? "selected" : ""}>all messages</option>
            </select></label>
          <label class="field"><span class="lab">Model scan budget per request (ms)</span>
            <input id="dlp-mbudget" type="number" min="100" value="${Number(d.model_scan_budget_ms) || 2000}" /></label>
          <label class="field span-2"><span class="lab">Sidecar URLs (one per line — overrides the single URL above)</span>
            <textarea id="dlp-murls" rows="2" class="mono" placeholder="http://dlp-bert:8000">${esc((d.model_urls || []).join("\n"))}</textarea></label>
        </div>
        <div class="hint">Sensitive Info Detection: choose which patterns are detected. Secrets are on by default; PII is opt-in. Model patterns need the BERT sidecar enabled above.</div>
        ${patGroups}
        <div class="form-sect">Custom patterns (regex)</div>
        <div id="custom-rows">${customRows}</div>
        <button class="btn ghost sm" id="add-custom" type="button">+ Add pattern</button>
        <div class="panel-actions"><button class="btn" id="dlp-save">Save DLP policy &amp; detection</button></div>
      </div>
    </div>`;

  const capturePanel = !capR.ok ? failBox("Capture & flywheel", [capR]) : `
    <div class="panel"><div class="panel-head"><h2>Capture &amp; flywheel</h2></div>
      <div class="panel-body">
        <div class="hint">Records prompts (redacted by default) for audit and DLP model training. Off by default.</div>
        <div class="form-grid">
          <label class="field check"><input type="checkbox" id="cap-en" ${cap.enabled ? "checked" : ""} /><span class="lab">Enable capture</span></label>
          <label class="field check"><input type="checkbox" id="cap-red" ${cap.redact ? "checked" : ""} /><span class="lab">Redact stored bodies</span></label>
          <label class="field"><span class="lab">Sample rate (0–1, incidents always captured)</span>
            <input id="cap-rate" type="number" step="0.05" min="0" max="1" value="${cap.sample_rate ?? 0}" /></label>
          <label class="field"><span class="lab">Retention (days)</span>
            <input id="cap-ret" type="number" min="1" value="${cap.retention_days ?? 30}" /></label>
        </div>
        <div class="hint">Raw training window — stores a short-lived un-redacted copy so the flywheel scans aligned text. Stores real secrets (encrypted) until the TTL.</div>
        <div class="form-grid">
          <label class="field check"><input type="checkbox" id="cap-raw" ${cap.raw_training ? "checked" : ""} /><span class="lab">Enable raw training window</span></label>
          <label class="field"><span class="lab">Raw copy TTL (hours)</span>
            <input id="cap-rawttl" type="number" min="1" value="${cap.raw_ttl_hours ?? 24}" /></label>
        </div>
        <div class="panel-actions"><button class="btn" id="cap-save">Save capture</button></div>
      </div>
    </div>`;

  const secondpassPanel = !spR.ok ? failBox("Second-pass re-scan", [spR]) : `
    <div class="panel"><div class="panel-head"><h2>Second-pass re-scan</h2></div>
      <div class="panel-body">
        <div class="hint">Off-hot-path job that re-scans captures with a model alias, confirms or clears detections and reports misses (webhook events <span class="mono">dlp.false_negative</span> / <span class="mono">dlp.alert_cleared</span>).</div>
        <div class="form-grid">
          <label class="field check"><input type="checkbox" id="sp-en" ${sp.enabled ? "checked" : ""} /><span class="lab">Enable second-pass</span></label>
          <label class="field"><span class="lab">Model alias used to scan</span>
            <input id="sp-model" value="${esc(sp.model || "")}" placeholder="e.g. sonnet" /></label>
          <label class="field"><span class="lab">Interval (seconds, applied at start)</span>
            <input id="sp-int" type="number" min="1" value="${Number(sp.interval_sec) || 60}" /></label>
          <label class="field"><span class="lab">Min score (0–1)</span>
            <input id="sp-score" type="number" step="0.05" min="0" max="1" value="${sp.min_score ?? 0.7}" /></label>
          <label class="field check span-2"><input type="checkbox" id="sp-raw" ${sp.allow_raw ? "checked" : ""} /><span class="lab">Allow raw: send the un-redacted raw window to the scanning model (it may be a third-party provider)</span></label>
        </div>
        <div class="panel-actions"><button class="btn" id="sp-save">Save second-pass</button></div>
      </div>
    </div>`;

  const hooks = (whR.data && whR.data.webhooks) || [];
  const hooksPanel = !whR.ok ? failBox("Alert webhooks", [whR]) :
    `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-hook">New alert webhook</button></div>` +
    panelTable("Alert webhooks", ["Name", "URL", "Events", "Secret", "Enabled", ""],
      hooks.map((h) => `<tr><td>${esc(h.name) || "—"}</td><td class="mono trunc" title="${esc(h.url)}">${esc(h.url)}</td>
        <td class="mono">${(h.events || []).map(esc).join(", ")}</td>
        <td>${h.has_secret ? `<span class="badge active">set</span>` : `<span class="badge neutral">none</span>`}</td>
        <td>${h.enabled ? "yes" : "no"}</td>
        <td class="actions"><button class="btn ghost sm" data-edithook='${esc(JSON.stringify(h))}'>Edit</button>
          <button class="btn danger sm" data-delhook="${esc(h.id)}" data-name="${esc(h.name || h.url)}">Delete</button></td></tr>`));
  const incidents = (incR.data && incR.data.incidents) || [];
  const incidentsPanel = !incR.ok ? failBox("Recent incidents", [incR]) :
    panelTable("Recent incidents", ["Time", "User", "Ingress", "Model", "Action", "Labels", "Matches", "Sample"],
      incidents.map((i) => `<tr><td>${fmtTime(i.ts)}</td><td>${esc(i.user) || "—"}</td><td>${esc(i.ingress)}</td>
        <td class="mono">${esc(i.alias)}</td>
        <td><span class="badge ${actBadge(i.action)}">${esc(i.action)}</span></td>
        <td>${(i.labels || []).map(esc).join(", ")}</td><td>${fmtNum(i.match_count)}</td>
        <td class="mono trunc" title="${esc(i.sample)}">${esc(i.sample)}</td></tr>`));

  c.innerHTML = policyPanel + capturePanel + secondpassPanel + hooksPanel + incidentsPanel;
  c.querySelectorAll("[data-retry]").forEach((b) => b.addEventListener("click", () => adminDLP(c)));

  if (policyOK) {
    // One PUT carries policy + patterns + custom patterns, gathered from the
    // form that was rendered from the stored config.
    const gatherDLP = () => {
      const patterns = {};
      document.querySelectorAll(".pat-tog").forEach((t) => { patterns[t.getAttribute("data-label")] = t.checked; });
      const custom_patterns = [];
      document.querySelectorAll(".custom-row").forEach((r) => {
        const label = r.querySelector(".cp-label").value.trim();
        const regex = r.querySelector(".cp-regex").value;
        if (label || regex) custom_patterns.push({ label, regex, enabled: r.querySelector(".cp-en").checked });
      });
      return {
        enabled: $("#dlp-en").checked, action: $("#dlp-act").value, scan_responses: false,
        model_enabled: $("#dlp-men").checked, model_url: $("#dlp-murl").value.trim(),
        model_min_score: Number($("#dlp-mscore").value) || 0,
        model_urls: $("#dlp-murls").value.split("\n").map((s) => s.trim()).filter(Boolean),
        model_max_concurrency: Number($("#dlp-mconc").value) || 0,
        model_scan_scope: $("#dlp-mscope").value, model_scan_budget_ms: Number($("#dlp-mbudget").value) || 2000,
        patterns, custom_patterns,
      };
    };
    $("#dlp-save").addEventListener("click", async () => {
      const x = await api("PUT", "/api/admin/dlp", gatherDLP());
      if (x.ok) toast("DLP policy saved"); else toast(apiError(x), "err");
    });
    $("#pat-all").addEventListener("click", () => {
      document.querySelectorAll(".pat-tog").forEach((t) => { t.checked = true; });
    });
    const wireDel = (btn) => btn.addEventListener("click", () => btn.closest(".custom-row").remove());
    document.querySelectorAll(".cp-del").forEach(wireDel);
    $("#add-custom").addEventListener("click", () => {
      const wrap = document.createElement("div");
      wrap.innerHTML = customRow({ enabled: true });
      const row = wrap.firstElementChild;
      $("#custom-rows").appendChild(row);
      wireDel(row.querySelector(".cp-del"));
    });
  }
  if (capR.ok) {
    $("#cap-save").addEventListener("click", async () => {
      const x = await api("PUT", "/api/admin/capture", {
        enabled: $("#cap-en").checked,
        sample_rate: Number($("#cap-rate").value) || 0,
        redact: $("#cap-red").checked,
        retention_days: Number($("#cap-ret").value) || 30,
        raw_training: $("#cap-raw").checked,
        raw_ttl_hours: Number($("#cap-rawttl").value) || 24,
      });
      if (x.ok) toast("Capture config saved"); else toast(apiError(x), "err");
    });
  }
  if (spR.ok) {
    $("#sp-save").addEventListener("click", async () => {
      const x = await api("PUT", "/api/admin/secondpass", {
        enabled: $("#sp-en").checked, model: $("#sp-model").value.trim(),
        interval_sec: Number($("#sp-int").value) || 60,
        min_score: Number($("#sp-score").value) || 0,
        allow_raw: $("#sp-raw").checked,
      });
      if (x.ok) toast("Second-pass config saved"); else toast(apiError(x), "err");
    });
  }
  if (whR.ok) {
    $("#new-hook").addEventListener("click", () => editWebhook(c));
    document.querySelectorAll("[data-edithook]").forEach((b) =>
      b.addEventListener("click", () => editWebhook(c, JSON.parse(b.getAttribute("data-edithook")))));
    document.querySelectorAll("[data-delhook]").forEach((b) => b.addEventListener("click", async () => {
      if (!confirm(`Delete webhook "${b.getAttribute("data-name")}"?`)) return;
      const x = await api("DELETE", `/api/admin/webhooks/${encodeURIComponent(b.getAttribute("data-delhook"))}`);
      if (x.ok) { toast("Deleted"); adminDLP(c); } else toast(apiError(x), "err");
    }));
  }
}

// webhookForm describes the alert-webhook form: a new webhook when h has no
// id, otherwise an edit of h prefilled from the list row. The secret cannot
// be read back, so on edit it starts blank and a blank secret keeps the
// stored one (the PUT leaves it untouched). Top-level so
// web/webhookform.test.mjs can extract it.
function webhookForm(h) {
  const edit = !!(h && h.id);
  const known = ["dlp.incident", "dlp.false_negative", "dlp.alert_cleared"];
  const events = edit ? (h.events || []) : ["dlp.incident"];
  let secretLabel = "Signing secret (HMAC, optional)";
  if (edit) secretLabel = h.has_secret ? "Signing secret (set — leave empty to keep)" : "Signing secret (none — leave empty to keep none)";
  return {
    title: edit ? `Edit webhook ${h.name || h.url}` : "New alert webhook",
    method: edit ? "PUT" : "POST",
    path: edit ? `/api/admin/webhooks/${encodeURIComponent(h.id)}` : "/api/admin/webhooks",
    fields: [
      { name: "name", label: "Name", value: edit ? h.name || "" : "" },
      { name: "url", label: "URL", value: edit ? h.url || "" : "" },
      { name: "secret", label: secretLabel, type: "password", value: "",
        placeholder: edit && h.has_secret ? "•••••• stored" : "" },
      // An event the console does not know yet stays listed so an edit
      // cannot drop it silently.
      { name: "events", label: "Events", type: "checkgroup", options: [...new Set([...known, ...events])], value: events },
      { name: "enabled", label: "Enabled", type: "checkbox", value: edit ? !!h.enabled : true },
    ],
  };
}

function editWebhook(c, h) {
  const f = webhookForm(h);
  modalForm(f.title, f.fields, async (v) => {
    if (!v.url) { toast("URL is required", "err"); return false; }
    if (!v.events.length) { toast("Pick at least one event", "err"); return false; }
    const x = await api(f.method, f.path, {
      name: v.name, url: v.url, secret: v.secret, enabled: v.enabled, events: v.events,
    });
    if (x.ok) { toast(f.method === "PUT" ? "Webhook saved" : "Webhook created"); adminDLP(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

// ---------- generic table + modal ----------
// breakdownTables renders the by-provider and by-model usage breakdown
// tables from a GET /api/usage/breakdown (or /api/admin/usage/breakdown)
// response.
function breakdownTables(d) {
  const tok = (n) => (+n || 0).toLocaleString("en-US");
  const provRows = (d.providers || []).map((p) => `<tr>
    <td>${esc(p.provider)}</td><td>${fmtNum(p.requests)}</td>
    <td class="mono">${tok(p.tokens_in)} / ${tokensOut(p.tokens_out, p.tokens_reasoning)}</td>
    <td>${fmtUSD(p.cost_usd)}</td><td>${fmtNum(p.p95_ms)} ms</td><td>${fmtNum(p.errors)}</td></tr>`);
  const modelRows = (d.models || []).map((m) => `<tr>
    <td class="mono">${esc(m.alias)}</td><td>${esc(m.provider)}</td><td class="mono">${esc(m.upstream_model)}</td>
    <td>${fmtNum(m.requests)}</td><td class="mono">${tok(m.tokens_in)} / ${tokensOut(m.tokens_out, m.tokens_reasoning)}</td>
    <td>${fmtUSD(m.cost_usd)}</td>
    <td>${fmtNum(m.p95_ms)} ms</td><td>${fmtNum(m.errors)}</td></tr>`);
  const empty = (title) => `<div class="panel">
    <div class="panel-head"><h2>${esc(title)}</h2></div>
    <div class="empty">No traffic in this window.</div></div>`;
  return (provRows.length
    ? panelTable("By provider", ["Provider", "Requests", "Tokens in / out", "Cost", "p95", "Errors"], provRows)
    : empty("By provider"))
    + (modelRows.length
    ? panelTable("By model", ["Alias", "Provider", "Upstream model", "Requests", "Tokens in / out", "Cost", "p95", "Errors"], modelRows)
    : empty("By model"));
}

function panelTable(title, cols, rowsHtml) {
  return `<div class="panel">
    <div class="panel-head"><h2>${esc(title)}</h2></div>
    ${rowsHtml.length === 0 ? `<div class="empty">Nothing here yet.</div>` : `
    <div class="table-scroll"><table><thead><tr>${cols.map((c) => `<th>${esc(c)}</th>`).join("")}</tr></thead>
    <tbody>${rowsHtml.join("")}</tbody></table></div>`}
  </div>`;
}

// wireModalDismiss gives a modal backdrop the usual ways out and returns
// its close function. A click closes only when it both started and ended on
// the backdrop (a text selection dragged out of an input must not count)
// and nothing was edited; Esc closes the topmost modal, asking first when
// there are unsaved edits.
function wireModalDismiss(bg) {
  let dirty = false, downOnBg = false;
  const close = () => { document.removeEventListener("keydown", onKey); bg.remove(); };
  const onKey = (e) => {
    if (e.key !== "Escape" || [...document.querySelectorAll(".modal-bg")].pop() !== bg) return;
    if (dirty && !confirm("Discard unsaved changes?")) return;
    close();
  };
  bg.addEventListener("input", () => { dirty = true; });
  bg.addEventListener("change", () => { dirty = true; });
  bg.addEventListener("mousedown", (e) => { downOnBg = e.target === bg; });
  bg.addEventListener("click", (e) => {
    if (downOnBg && e.target === bg && !dirty) close();
    downOnBg = false;
  });
  document.addEventListener("keydown", onKey);
  return close;
}

// modalForm renders a small form in a modal. A field may carry
// `showWhen(values)`, in which case it is shown only while that predicate
// holds — enough for a form whose shape depends on one of its own fields,
// like a provider kind that is addressed and authenticated differently from
// every other one. Hidden fields stay in the DOM and are still submitted,
// so the caller decides what a hidden value means.
// renderFormField renders one modalForm field spec to its HTML string.
// Pulled out as its own top-level function (same reason staleGuard is one —
// see web/staleguard.test.mjs) so the field-type branches, including the
// type:"number" min/max/step path, are independently testable without a DOM.
function renderFormField(f) {
  const open = `<label class="field" data-field="${f.name}"><span class="lab">${esc(f.label)}</span>`;
  if (f.type === "checkbox") {
    return `<label class="field check" data-field="${f.name}">
      <input type="checkbox" name="${f.name}" ${f.value ? "checked" : ""} /><span class="lab">${esc(f.label)}</span></label>`;
  }
  if (f.type === "checkgroup") {
    // A set of known values picked with checkboxes; f.value is the array of
    // selected ones.
    const sel = new Set(f.value || []);
    return `<div class="field" data-field="${f.name}"><span class="lab">${esc(f.label)}</span>
      <div class="checkgroup">${(f.options || []).map((o) => `<label class="check">
        <input type="checkbox" data-group="${f.name}" value="${esc(o)}" ${sel.has(o) ? "checked" : ""} /><span class="mono">${esc(o)}</span></label>`).join("")
      }</div></div>`;
  }
  if (f.type === "limits") {
    // Role limits ({dim: {window: N}}) as a dimension × window grid of
    // number inputs; an empty cell is no limit.
    const lim = f.value || {};
    const dims = [["tokens", "Tokens"], ["cost_usd", "Cost $"], ["audio_seconds", "Audio sec"], ["tts_chars", "TTS chars"]];
    const wins = ["5h", "24h", "7d"];
    return `<div class="field" data-field="${f.name}"><span class="lab">${esc(f.label)}</span>
      <div class="limits-grid"><span></span>${wins.map((w) => `<span class="lab">${w}</span>`).join("")}
      ${dims.map(([d, label]) => `<span class="lab">${label}</span>${wins.map((w) => {
        const v = lim[d] && lim[d][w] != null ? lim[d][w] : "";
        return `<input type="number" min="0" step="any" data-limit="${d}.${w}" value="${esc(v)}" aria-label="${label} per ${w}" />`;
      }).join("")}`).join("")}</div></div>`;
  }
  if (f.type === "budgets") {
    // A user's budgets ({tokens|cost_usd: {day|week|month: N}}) in the same
    // grid style as role limits; 0 is shown as an empty cell (no cap).
    const b = f.value || {};
    const units = [["tokens", "Tokens", "1"], ["cost_usd", "Cost $", "any"]];
    const periods = [["day", "Day"], ["week", "Week"], ["month", "Month"]];
    return `<div class="field" data-field="${f.name}"><span class="lab">${esc(f.label)}</span>
      <div class="limits-grid budget-grid"><span></span>${periods.map(([, pl]) => `<span class="lab">${pl}</span>`).join("")}
      ${units.map(([u, label, step]) => `<span class="lab">${label}</span>${periods.map(([p, pl]) => {
        const n = Number(b[u] && b[u][p]) || 0;
        return `<input type="number" min="0" step="${step}" data-budget="${u}.${p}" value="${n > 0 ? esc(n) : ""}" placeholder="no cap" aria-label="${label} per ${pl.toLowerCase()}" />`;
      }).join("")}`).join("")}</div></div>`;
  }
  if (f.type === "textarea") {
    return `${open}
      <textarea name="${f.name}">${esc(f.value)}</textarea></label>`;
  }
  if (f.type === "select") {
    return `${open}
      <select name="${f.name}" ${f.disabled ? "disabled" : ""}>${(f.options || []).map((o) => {
        const val = typeof o === "object" ? o.value : o;
        const label = typeof o === "object" ? o.label : o;
        return `<option value="${esc(val)}" ${val === f.value ? "selected" : ""}>${esc(label)}</option>`;
      }).join("")}</select></label>`;
  }
  if (f.type === "password") {
    return `${open}
      <input type="password" name="${f.name}" value="${esc(f.value)}" autocomplete="new-password"
        placeholder="${esc(f.placeholder || "")}" /></label>`;
  }
  if (f.type === "number") {
    // min/max/step give the browser's own constraint validation a shot
    // at a bad value (e.g. a negative max_concurrency) BEFORE Save ever
    // round-trips to the backend to find out the hard way.
    const attrs = [
      f.min !== undefined ? `min="${esc(f.min)}"` : "",
      f.max !== undefined ? `max="${esc(f.max)}"` : "",
      f.step !== undefined ? `step="${esc(f.step)}"` : "",
    ].filter(Boolean).join(" ");
    return `${open}
      <input type="number" name="${f.name}" value="${esc(f.value)}" ${attrs} ${f.disabled ? "disabled" : ""} /></label>`;
  }
  return `${open}
    <input name="${f.name}" value="${esc(f.value)}" ${f.disabled ? "disabled" : ""} /></label>`;
}

function modalForm(title, fields, onSubmit) {
  const bg = document.createElement("div");
  bg.className = "modal-bg";
  bg.innerHTML = `<div class="modal"><h3>${esc(title)}</h3><form id="mf">
    ${fields.map(renderFormField).join("")}
    <div class="row modal-foot">
      <button type="button" class="btn ghost" id="mf-cancel">Cancel</button>
      <button type="submit" class="btn">Save</button>
    </div></form></div>`;
  document.body.appendChild(bg);
  const form = $("#mf", bg);
  const close = wireModalDismiss(bg);
  $("#mf-cancel", bg).addEventListener("click", close);

  const values = () => {
    const v = {};
    fields.forEach((f) => {
      if (f.type === "checkgroup") {
        v[f.name] = [...form.querySelectorAll(`[data-group="${f.name}"]:checked`)].map((el) => el.value);
      } else if (f.type === "limits") {
        // Start from the stored object so a dimension or window the grid
        // does not show is kept; every shown cell is rewritten.
        const out = JSON.parse(JSON.stringify(f.value || {}));
        form.querySelectorAll("[data-limit]").forEach((el) => {
          const [d, w] = el.getAttribute("data-limit").split(".");
          out[d] = out[d] || {};
          if (el.value.trim() === "") delete out[d][w];
          else out[d][w] = Number(el.value);
          if (!Object.keys(out[d]).length) delete out[d];
        });
        v[f.name] = out;
      } else if (f.type === "budgets") {
        // Raw cell strings; the caller parses and validates them.
        v[f.name] = {};
        form.querySelectorAll("[data-budget]").forEach((el) => { v[f.name][el.getAttribute("data-budget")] = el.value; });
      } else {
        const el = form[f.name];
        v[f.name] = f.type === "checkbox" ? el.checked : el.value;
      }
    });
    return v;
  };
  const conditional = fields.filter((f) => f.showWhen);
  if (conditional.length) {
    const applyVisibility = () => {
      const v = values();
      conditional.forEach((f) => {
        $(`[data-field="${f.name}"]`, bg).hidden = !f.showWhen(v);
      });
    };
    applyVisibility();
    // Both events: `change` alone is enough for a <select>, but a field gated
    // on a typed number (a pricing threshold) would otherwise only reveal its
    // dependants once the operator blurred the box.
    form.addEventListener("change", applyVisibility);
    form.addEventListener("input", applyVisibility);
  }

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const ok = await onSubmit(values());
    if (ok !== false) close();
  });
}

init();
