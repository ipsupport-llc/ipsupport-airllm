"use strict";

// ---------- helpers ----------
const $ = (sel, root = document) => root.querySelector(sel);
const app = $("#app");

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  let data = null;
  try { data = await res.json(); } catch (_) { /* no body */ }
  return { ok: res.ok, status: res.status, data };
}

function toast(msg, kind = "ok") {
  const t = document.createElement("div");
  t.className = "toast " + kind;
  t.textContent = msg;
  $("#toasts").appendChild(t);
  setTimeout(() => t.remove(), 3500);
}

function fmtUSD(n) { return "$" + Number(n || 0).toFixed(4); }
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
const ADMIN_TABS = ["users", "keys", "usage", "roles", "aliases", "providers", "pricing", "dlp", "audit"];

function renderShell() {
  const adminLink = me.is_admin ? `<div class="sect">Admin</div><a href="#/admin/users" data-nav>Admin console</a>` : "";
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
          <div class="who">${esc(me.subject)}</div>
          <div>${esc((me.roles || []).join(", ")) || "no roles"}</div>
          ${authMode.mode !== "oidc" ? `<button class="btn ghost sm" id="change-pw" style="margin-top:.3rem">Change password</button>` : ""}
          <button class="btn ghost sm" id="logout" style="margin-top:.6rem">Sign out</button>
          <div class="copyright">© 2026 <a href="https://ipsupport.us" target="_blank" rel="noopener">IPSupport LLC</a></div>
        </div>
      </aside>
      <main class="main"><div id="view"></div></main>
    </div>`;
  const cpBtn = $("#change-pw");
  if (cpBtn) cpBtn.addEventListener("click", changePassword);
  $("#logout").addEventListener("click", async () => { await api("POST", "/auth/logout"); me = null; renderLogin(); });
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
  } else if (hash === "#/keys") viewKeys(view);
  else if (hash === "#/usage") viewUsage(view);
  else viewDashboard(view);
}

// ---------- dashboard ----------
async function viewDashboard(view) {
  view.innerHTML = `<h1 class="page-title">Dashboard</h1><div id="dash"></div>`;
  const seriesPath = me.is_admin ? "/api/admin/usage/series" : "/api/usage/series";
  const [u, k, sr] = await Promise.all([api("GET", "/api/usage"), api("GET", "/api/keys"), api("GET", seriesPath)]);
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
  const sparksHtml = series.length
    ? `<div class="cards" style="margin-top:1rem">
        ${spark("Tokens/hr", "tokens", fmtNum)}
        ${spark("Cost/hr", "cost_usd", (v) => "$" + Number(v).toFixed(4))}
        ${spark("Requests/hr", "requests", fmtNum)}
        ${spark("p95 latency", "p95_ms", (v) => fmtNum(v) + " ms")}
      </div>`
    : `<div class="empty" style="margin-top:1rem">No usage history yet.</div>`;
  $("#dash").innerHTML = `
    ${usageCards(usage)}
    ${sparksHtml}
    <div class="panel">
      <div class="panel-head"><h2>Your keys</h2><a class="btn sm" href="#/keys">Manage</a></div>
      <div class="empty">${active} active key${active === 1 ? "" : "s"} of ${keys.length} total.</div>
    </div>
    ${connectPanel()}`;
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
async function viewUsage(view) {
  view.innerHTML = `<h1 class="page-title">Usage</h1><div id="u"></div>`;
  const [u, b] = await Promise.all([
    api("GET", "/api/usage"),
    api("GET", "/api/usage/breakdown?hours=24"),
  ]);
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
            <input type="number" id="cap-limit" value="50" min="1" max="1000" />
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

async function loadCaptures() {
  const status = ($("#cap-status") && $("#cap-status").value) || "";
  const limit = ($("#cap-limit") && $("#cap-limit").value) || "50";
  const fromEl = $("#cap-from");
  const toEl = $("#cap-to");

  const params = new URLSearchParams();
  if (status) params.set("review_status", status);
  if (limit) params.set("limit", limit);
  if (fromEl && fromEl.value) params.set("from", new Date(fromEl.value).toISOString());
  if (toEl && toEl.value) params.set("to", new Date(toEl.value).toISOString());

  const r = await api("GET", "/api/audit/captures?" + params.toString());
  const captures = (r.data && r.data.captures) || [];
  const el = $("#cap-results");
  if (!el) return;

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
      <td><span class="badge ${c.status < 300 ? "active" : "revoked"}">${c.status || "?"}</span></td>
      <td>${c.redacted ? `<span class="badge neutral">yes</span>` : "no"}</td>
      <td>${labelBadges(c.detected)}</td>
      <td><span class="badge neutral">${esc(c.review_status || "unreviewed")}</span></td>
    </tr>`));

  el.querySelectorAll("[data-cid]").forEach((row) =>
    row.addEventListener("click", () => openCaptureDrawer(row.getAttribute("data-cid"))));
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

  // Build transcript HTML.
  let bodyHtml = "";
  if (body) {
    try {
      const parsed = JSON.parse(body);
      const msgs = parsed.messages || [];
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

  // Detected spans summary.
  const spansHtml = detected.length
    ? `<div style="margin-bottom:.5rem">${detected.map((f) =>
        `<span class="badge revoked" title="start:${f.start} end:${f.end}">${esc(f.label)}</span>`).join(" ")}</div>`
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
      <div class="lab" style="margin-bottom:.3rem">Add missed span</div>
      <div class="row" style="flex-wrap:wrap;gap:.4rem;align-items:flex-end;margin-bottom:.5rem">
        <label class="field" style="flex:0 1 160px;margin:0">
          <span class="lab">Label</span>
          <input id="rev-label" placeholder="e.g. openai_key" />
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

  // Status buttons (no extra span).
  modal.querySelectorAll("[data-rev-status]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const status = btn.getAttribute("data-rev-status");
      const res = await api("POST", `/api/audit/captures/${encodeURIComponent(id)}/review`,
        { review_status: status, labels: [] });
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
      { review_status: "false_negative", labels: [{ label, start, end }] });
    if (res.ok) {
      toast("Submitted as false_negative");
      modal.innerHTML = "";
      loadReviewQueue();
    } else {
      errEl.textContent = (res.data && res.data.error) || "Failed";
    }
  });
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

async function loadKeys() {
  const r = await api("GET", "/api/keys");
  const keys = (r.data && r.data.keys) || [];
  $("#keys-panel").innerHTML = `
    <div class="panel">
      <div class="panel-head"><h2>${keys.length} key${keys.length === 1 ? "" : "s"}</h2></div>
      ${keys.length === 0 ? `<div class="empty">No keys yet. Create one above.</div>` : `
      <table><thead><tr><th>Name</th><th>Key</th><th>Status</th><th>Created</th><th>Last used</th><th></th></tr></thead>
      <tbody>${keys.map(keyRow).join("")}</tbody></table>`}
    </div>`;
  document.querySelectorAll("[data-revoke]").forEach((b) =>
    b.addEventListener("click", () => revokeKey(b.getAttribute("data-revoke"))));
}

function keyRow(k) {
  const revoke = k.status === "active"
    ? `<button class="btn danger sm" data-revoke="${k.id}">Revoke</button>` : "";
  return `<tr>
    <td>${esc(k.name)}</td>
    <td class="mono">${esc(k.prefix)}…${esc(k.last4)}</td>
    <td><span class="badge ${k.status}">${k.status}</span></td>
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
    navigator.clipboard.writeText(r.data.token).then(() => toast("Copied to clipboard"));
  });
  toast("Key created");
  loadKeys();
}

async function revokeKey(id) {
  if (!confirm("Revoke this key? Clients using it will stop working.")) return;
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
    users: adminUsers, keys: adminKeys, usage: adminUsage, roles: adminRoles,
    aliases: adminAliases, providers: adminProviders, pricing: adminPricing,
    dlp: adminDLP, audit: adminAudit,
  }[tab] || adminUsers)(c);
}

async function adminUsage(c) {
  const [u, b] = await Promise.all([
    api("GET", "/api/admin/usage"),
    api("GET", "/api/admin/usage/breakdown?hours=24"),
  ]);
  c.innerHTML = `<h2 style="font-size:1.05rem">Organization usage</h2>` + usageCards(u.data || {}) +
    breakdownTables(b.data || {});
}

async function adminUsers(c) {
  const r = await api("GET", "/api/admin/users");
  const users = (r.data && r.data.users) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-user">New user</button></div>` +
    panelTable("Users", ["Subject", "Email", "Roles", "Auth", "Status", "Created", ""],
      users.map((u) => `<tr>
        <td>${esc(u.subject)}</td>
        <td>${esc(u.email)}</td>
        <td>${(u.roles || []).map((x) => `<span class="badge ${x === "airllm_admin" ? "admin" : "neutral"}">${esc(x)}</span>`).join(" ")}</td>
        <td>${esc(u.auth_source || "local")}</td>
        <td>${u.disabled ? `<span class="badge revoked">disabled</span>` : `<span class="badge active">active</span>`}</td>
        <td>${fmtTime(u.created_at)}</td>
        <td style="text-align:right">
          <button class="btn ghost sm" data-edit-user='${esc(JSON.stringify(u))}'>Edit</button>
          <button class="btn ghost sm" data-reset-user="${esc(u.id)}">Reset pw</button>
          <button class="btn danger sm" data-del-user="${esc(u.id)}">Delete</button>
        </td></tr>`));
  $("#new-user").addEventListener("click", () => editUser(c, null));
  document.querySelectorAll("[data-edit-user]").forEach((b) =>
    b.addEventListener("click", () => editUser(c, JSON.parse(b.getAttribute("data-edit-user")))));
  document.querySelectorAll("[data-reset-user]").forEach((b) =>
    b.addEventListener("click", () => resetUserPassword(c, b.getAttribute("data-reset-user"))));
  document.querySelectorAll("[data-del-user]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm("Delete user " + b.getAttribute("data-del-user") + "?")) return;
    const x = await api("DELETE", `/api/admin/users/${encodeURIComponent(b.getAttribute("data-del-user"))}`);
    if (x.ok) { toast("User deleted"); adminUsers(c); }
    else toast((x.data && x.data.error) || "Failed", "err");
  }));
}

async function editUser(c, u) {
  const rolesR = await api("GET", "/api/admin/roles");
  const availRoles = ((rolesR.data && rolesR.data.roles) || []).map((rp) => rp.role);
  const hint = availRoles.length ? ` (available: ${availRoles.join(", ")})` : "";
  const isNew = !u;
  const fields = [];
  if (isNew) fields.push({ name: "username", label: "Username", value: "" });
  fields.push(
    { name: "email", label: "Email", value: u ? (u.email || "") : "" },
    { name: "display", label: "Display name", value: u ? (u.display || "") : "" },
    { name: "roles", label: `Roles (comma-separated)${hint}`, value: u ? (u.roles || []).join(", ") : "" },
    { name: "disabled", label: "Disabled", type: "checkbox", value: u ? !!u.disabled : false },
  );
  if (isNew) fields.push({ name: "password", label: "Password", type: "password", value: "" });
  modalForm(isNew ? "New user" : `Edit user ${u.subject}`, fields, async (v) => {
    const roles = v.roles.split(",").map((s) => s.trim()).filter(Boolean);
    let x;
    if (isNew) {
      x = await api("POST", "/api/admin/users", {
        username: v.username, email: v.email, display: v.display, roles, password: v.password,
      });
    } else {
      x = await api("PUT", `/api/admin/users/${encodeURIComponent(u.id)}`, {
        email: v.email, display: v.display, roles, disabled: v.disabled,
      });
    }
    if (x.ok) { toast(isNew ? "User created" : "User saved"); adminUsers(c); return true; }
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

async function adminKeys(c) {
  const r = await api("GET", "/api/admin/keys");
  const keys = (r.data && r.data.keys) || [];
  c.innerHTML = panelTable("All API keys", ["Owner", "Name", "Key", "Status", "Last used", ""],
    keys.map((k) => `<tr><td>${esc(k.owner)}</td><td>${esc(k.name)}</td>
      <td class="mono">${esc(k.prefix)}…${esc(k.last4)}</td>
      <td><span class="badge ${k.status}">${k.status}</span></td>
      <td>${fmtTime(k.last_used_at)}</td>
      <td style="text-align:right">${k.status === "active" ? `<button class="btn danger sm" data-arev="${k.id}">Revoke</button>` : ""}</td></tr>`));
  document.querySelectorAll("[data-arev]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm("Revoke this key?")) return;
    const x = await api("POST", `/api/admin/keys/${b.getAttribute("data-arev")}/revoke`);
    if (x.ok) { toast("Revoked"); adminKeys(c); } else toast("Failed", "err");
  }));
}

async function adminAudit(c) {
  const r = await api("GET", "/api/admin/audit");
  const rows = (r.data && r.data.audit) || [];
  c.innerHTML = panelTable("Audit log", ["Time", "Actor", "Action", "Target"],
    rows.map((e) => `<tr><td>${fmtTime(e.ts)}</td><td>${esc(e.actor)}</td>
      <td class="mono">${esc(e.action)}</td><td class="mono">${esc(e.target)}</td></tr>`));
}

async function adminRoles(c) {
  const r = await api("GET", "/api/admin/roles");
  const roles = (r.data && r.data.roles) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-role">New role</button></div>` +
    panelTable("Role policies", ["Role", "Allowed models", "Passthrough", "Limits", ""],
      roles.map((rp) => `<tr><td class="mono">${esc(rp.role)}</td>
        <td>${(rp.allowed_models || []).map(esc).join(", ")}</td>
        <td>${rp.allow_passthrough ? "yes" : "no"}</td>
        <td>${fmtLimits(rp.limits)}</td>
        <td style="text-align:right"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(rp))}'>Edit</button></td></tr>`));
  $("#new-role").addEventListener("click", () => editRole(c, {}));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editRole(c, JSON.parse(b.getAttribute("data-edit")))));
}

function editRole(c, rp) {
  modalForm(rp.role ? `Edit role ${rp.role}` : "New role", [
    { name: "role", label: "Role name", value: rp.role || "", disabled: !!rp.role },
    { name: "allowed_models", label: "Allowed models (comma; * = all)", value: (rp.allowed_models || []).join(", ") },
    { name: "allow_passthrough", label: "Allow explicit provider passthrough", type: "checkbox", value: !!rp.allow_passthrough },
    { name: "limits", label: "Limits (JSON)", type: "textarea", value: JSON.stringify(rp.limits || {}, null, 2) },
  ], async (v) => {
    let limits;
    try { limits = JSON.parse(v.limits || "{}"); } catch (_) { toast("Limits must be valid JSON", "err"); return false; }
    const body = {
      allowed_models: v.allowed_models.split(",").map((s) => s.trim()).filter(Boolean),
      allow_passthrough: v.allow_passthrough,
      limits,
    };
    const x = await api("PUT", `/api/admin/roles/${encodeURIComponent(v.role)}`, body);
    if (x.ok) { toast("Role saved"); adminRoles(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

async function adminProviders(c) {
  const r = await api("GET", "/api/admin/providers");
  const ps = (r.data && r.data.providers) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-prov">New provider</button></div>` +
    panelTable("Providers", ["Name", "Kind", "Base URL", "Key", "Concurrency", "Enabled", ""],
      ps.map((p) => `<tr><td class="mono">${esc(p.name)}</td><td>${esc(p.kind)}</td>
        <td class="mono">${esc(p.base_url) || "—"}</td>
        <td>${p.has_credential ? `<span class="badge active">set</span>` : `<span class="badge neutral">none</span>`}</td>
        <td>${p.max_concurrency > 0 ? p.max_concurrency : "∞"}</td>
        <td>${p.enabled ? "yes" : "no"}</td>
        <td style="text-align:right"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(p))}'>Edit</button></td></tr>`));
  $("#new-prov").addEventListener("click", () => editProvider(c, {}));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editProvider(c, JSON.parse(b.getAttribute("data-edit")))));
}

function editProvider(c, p) {
  modalForm(p.name ? `Edit provider ${p.name}` : "New provider", [
    { name: "name", label: "Name", value: p.name || "", disabled: !!p.name },
    { name: "kind", label: "Kind", type: "select", options: ["mock", "openai", "openrouter", "xai", "ollama", "anthropic"], value: p.kind || "mock" },
    { name: "base_url", label: "Base URL (optional override)", value: p.base_url || "" },
    { name: "api_key", label: p.has_credential ? "API key (set — blank keeps current)" : "API key", type: "password", value: "", placeholder: p.has_credential ? "•••••• stored" : "" },
    { name: "max_concurrency", label: "Max concurrency (0 = unlimited)", value: p.max_concurrency ?? 0 },
    { name: "enabled", label: "Enabled", type: "checkbox", value: p.enabled !== false },
  ], async (v) => {
    const x = await api("PUT", `/api/admin/providers/${encodeURIComponent(v.name)}`,
      { kind: v.kind, base_url: v.base_url, enabled: v.enabled, api_key: v.api_key, max_concurrency: Number(v.max_concurrency) || 0 });
    if (x.ok) { toast("Provider saved"); adminProviders(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

async function adminPricing(c) {
  const r = await api("GET", "/api/admin/pricing");
  const ps = (r.data && r.data.pricing) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-price">New price</button></div>` +
    panelTable("Pricing (USD / 1M tokens)", ["Model", "Input", "Output", ""],
      ps.map((p) => `<tr><td class="mono">${esc(p.model)}</td><td>${p.input_per_1m}</td><td>${p.output_per_1m}</td>
        <td style="text-align:right"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(p))}'>Edit</button></td></tr>`));
  $("#new-price").addEventListener("click", () => editPrice(c, {}));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editPrice(c, JSON.parse(b.getAttribute("data-edit")))));
}

function editPrice(c, p) {
  modalForm(p.model ? `Edit price ${p.model}` : "New price", [
    { name: "model", label: "Upstream model", value: p.model || "", disabled: !!p.model },
    { name: "input_per_1m", label: "Input $ / 1M", value: p.input_per_1m ?? 0 },
    { name: "output_per_1m", label: "Output $ / 1M", value: p.output_per_1m ?? 0 },
  ], async (v) => {
    const x = await api("PUT", `/api/admin/pricing/${encodeURIComponent(v.model)}`,
      { input_per_1m: Number(v.input_per_1m), output_per_1m: Number(v.output_per_1m) });
    if (x.ok) { toast("Pricing saved"); adminPricing(c); return true; }
    toast((x.data && x.data.error) || "Failed", "err"); return false;
  });
}

async function adminAliases(c) {
  const r = await api("GET", "/api/admin/aliases");
  const al = (r.data && r.data.aliases) || [];
  c.innerHTML = `<div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-alias">New alias</button></div>` +
    panelTable("Model aliases", ["Alias", "Protocol", "Strategy", "BERT", "Targets", ""],
      al.map((a) => `<tr><td class="mono">${esc(a.alias)}</td><td>${esc(a.protocol)}</td>
        <td>${esc(a.strategy || "round_robin")}</td>
        <td>${a.dlp_model_scan ? `<span class="badge neutral">on</span>` : `<span class="badge revoked">off</span>`}</td>
        <td class="mono">${(a.targets || []).map((t) => `${esc(t.provider)}/${esc(t.upstream_model)} (p${t.priority})`).join(", ") || "—"}</td>
        <td style="text-align:right"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(a))}'>Edit</button>
          <button class="btn danger sm" data-del="${esc(a.alias)}">Delete</button></td></tr>`));
  $("#new-alias").addEventListener("click", () => editAlias(c, {}));
  document.querySelectorAll("[data-edit]").forEach((b) =>
    b.addEventListener("click", () => editAlias(c, JSON.parse(b.getAttribute("data-edit")))));
  document.querySelectorAll("[data-del]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm("Delete alias " + b.getAttribute("data-del") + "?")) return;
    const x = await api("DELETE", `/api/admin/aliases/${encodeURIComponent(b.getAttribute("data-del"))}`);
    if (x.ok) { toast("Deleted"); adminAliases(c); } else toast("Failed", "err");
  }));
}

async function editAlias(c, a) {
  const pr = await api("GET", "/api/admin/providers");
  let providers = ((pr.data && pr.data.providers) || []).map((p) => p.name);
  if (providers.length === 0) providers = ["mock"];
  // Existing alias names, for the rename-collision guard on save.
  const ar = await api("GET", "/api/admin/aliases");
  const existingAliases = ((ar.data && ar.data.aliases) || []).map((x) => x.alias);

  const targets = (a.targets && a.targets.length)
    ? a.targets
    : [{ provider: providers[0], upstream_model: "", upstream_protocol: "openai" }];

  const bg = document.createElement("div");
  bg.className = "modal-bg";
  bg.innerHTML = `<div class="modal" style="max-width:560px">
    <h3>${esc(a.alias ? "Edit alias " + a.alias : "New alias")}</h3>
    <label class="field"><span class="lab">Alias (the model name clients request)</span>
      <input id="al-alias" value="${esc(a.alias || "")}" /></label>
    <label class="field"><span class="lab">Client protocol</span>
      <select id="al-proto">
        <option ${a.protocol !== "anthropic" ? "selected" : ""}>openai</option>
        <option ${a.protocol === "anthropic" ? "selected" : ""}>anthropic</option>
      </select></label>
    <label class="field"><span class="lab">Balancing within a priority tier</span>
      <select id="al-strategy">
        <option value="round_robin" ${a.strategy !== "least_busy" ? "selected" : ""}>round_robin</option>
        <option value="least_busy" ${a.strategy === "least_busy" ? "selected" : ""}>least_busy</option>
      </select></label>
    <label class="field"><span class="lab">Layer-2 BERT scan (fuzzy PII)</span>
      <input id="al-bert" type="checkbox" ${a.dlp_model_scan === false ? "" : "checked"} style="width:auto" /></label>
    <div class="lab" style="color:var(--muted);font-size:.82rem;margin-bottom:.3rem">Targets: same priority = load-balanced tier; higher number = fallback tier</div>
    <div id="al-targets"></div>
    <button type="button" class="btn ghost sm" id="al-add" style="margin-top:.3rem">+ Add target</button>
    <div class="row" style="justify-content:flex-end;margin-top:1rem">
      <button type="button" class="btn ghost" id="al-cancel">Cancel</button>
      <button type="button" class="btn" id="al-save">Save</button>
    </div>
  </div>`;
  document.body.appendChild(bg);
  const close = () => bg.remove();
  const tdiv = $("#al-targets", bg);

  const provOpts = (sel) => providers.map((p) =>
    `<option value="${esc(p)}" ${p === sel ? "selected" : ""}>${esc(p)}</option>`).join("");
  // Upstream model suggestions: fetched per provider, memoized (as an
  // in-flight promise, so concurrent requests for the same provider share
  // one fetch) for the lifetime of this modal. Failure or unsupported ->
  // empty list; the input stays free-text either way.
  const modelLists = {};
  let rowSeq = 0;
  function fetchModels(prov) {
    if (!(prov in modelLists)) {
      modelLists[prov] = api("GET", `/api/admin/providers/${encodeURIComponent(prov)}/models`)
        .then((r) => (r.ok && r.data && r.data.models) || [])
        .catch(() => []);
    }
    return modelLists[prov];
  }
  async function loadModels(prov, dl, provSel) {
    const models = await fetchModels(prov);
    // Stale guard: the user may have switched provider again while this
    // fetch was in flight — never overwrite a newer selection's list.
    if (provSel && provSel.value !== prov) return;
    dl.innerHTML = models.map((m) => `<option value="${esc(m)}">`).join("");
  }
  function addRow(t) {
    const row = document.createElement("div");
    row.className = "row tgt";
    row.style.marginBottom = ".4rem";
    row.innerHTML = `
      <input class="t-prio" type="number" min="0" value="${Number(t.priority) || 0}" title="priority / tier (same number = load-balanced)" style="width:64px" />
      <select class="t-prov" style="width:auto">${provOpts(t.provider)}</select>
      <input class="t-model" list="al-models-${rowSeq}" placeholder="upstream model" value="${esc(t.upstream_model || "")}" style="flex:1;min-width:120px" />
      <datalist id="al-models-${rowSeq}"></datalist>
      <select class="t-proto" style="width:auto">
        <option ${t.upstream_protocol !== "anthropic" ? "selected" : ""}>openai</option>
        <option ${t.upstream_protocol === "anthropic" ? "selected" : ""}>anthropic</option>
      </select>
      <button type="button" class="btn danger sm t-del" title="remove">×</button>`;
    row.querySelector(".t-del").addEventListener("click", () => row.remove());
    rowSeq++;
    const dl = row.querySelector("datalist");
    const provSel = row.querySelector(".t-prov");
    loadModels(provSel.value, dl, provSel);
    provSel.addEventListener("change", () => {
      // Clear the stale model name: it belongs to the previous provider, and
      // the browser filters datalist suggestions by the input's text — with
      // the old name left in place the refreshed list appears empty.
      row.querySelector(".t-model").value = "";
      loadModels(provSel.value, dl, provSel);
    });
    tdiv.appendChild(row);
  }
  targets.forEach(addRow);
  $("#al-add", bg).addEventListener("click", () =>
    addRow({ provider: providers[0], upstream_model: "", upstream_protocol: "openai" }));
  $("#al-cancel", bg).addEventListener("click", close);
  bg.addEventListener("click", (e) => { if (e.target === bg) close(); });
  $("#al-save", bg).addEventListener("click", async () => {
    const alias = $("#al-alias", bg).value.trim();
    if (!alias) { toast("Alias is required", "err"); return; }
    // PUT is an upsert: renaming (or creating) onto another existing alias
    // would silently overwrite it. Block the collision instead.
    if (alias !== a.alias && existingAliases.includes(alias)) {
      toast(`Alias ${alias} already exists`, "err"); return;
    }
    const tlist = [...tdiv.querySelectorAll(".tgt")].map((r) => ({
      priority: Number(r.querySelector(".t-prio").value) || 0,
      provider: r.querySelector(".t-prov").value,
      upstream_model: r.querySelector(".t-model").value.trim(),
      upstream_protocol: r.querySelector(".t-proto").value,
    })).filter((t) => t.upstream_model);
    if (tlist.length === 0) { toast("Add at least one target with a model", "err"); return; }
    const x = await api("PUT", `/api/admin/aliases/${encodeURIComponent(alias)}`,
      { protocol: $("#al-proto", bg).value, strategy: $("#al-strategy", bg).value, targets: tlist,
        dlp_model_scan: $("#al-bert", bg).checked });
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
  const [cfgR, incR, whR, capR, patR] = await Promise.all([
    api("GET", "/api/admin/dlp"),
    api("GET", "/api/admin/dlp/incidents"),
    api("GET", "/api/admin/webhooks"),
    api("GET", "/api/admin/capture"),
    api("GET", "/api/admin/dlp/patterns"),
  ]);
  const d = cfgR.data || { enabled: false, action: "off" };
  const incidents = (incR.data && incR.data.incidents) || [];
  const hooks = (whR.data && whR.data.webhooks) || [];
  const cap = capR.data || { enabled: false, sample_rate: 0, redact: true, retention_days: 30, raw_training: false, raw_ttl_hours: 24 };
  const actBadge = (a) => a === "blocked" ? "revoked" : (a === "redacted" ? "admin" : "neutral");

  // Sensitive Info Detection (guardrails): build grouped pattern toggles.
  const catalog = (patR.data && patR.data.patterns) || [];
  const curPat = d.patterns || {};
  const patOn = (p) => (p.label in curPat) ? curPat[p.label] : p.default_on;
  const togRow = (p) => `<label class="field" style="display:flex;align-items:center;gap:.5rem;margin-bottom:.4rem">
      <input type="checkbox" class="pat-tog" data-label="${esc(p.label)}" ${patOn(p) ? "checked" : ""} style="width:auto" />
      <span class="mono" style="font-size:.85rem">${esc(p.label)}${p.model ? ` <span style="color:var(--muted)">· adds latency</span>` : ""}</span></label>`;
  const group = (title, items) => items.length ? `<div class="lab" style="color:var(--muted);font-size:.78rem;text-transform:uppercase;margin:.7rem 0 .35rem">${title}</div>${items.map(togRow).join("")}` : "";
  const patGroups =
    group("Secrets", catalog.filter((p) => p.category === "secret")) +
    group("PII", catalog.filter((p) => p.category === "pii" && !p.model)) +
    group("Model (BERT sidecar)", catalog.filter((p) => p.model));
  const customRow = (cp) => `<div class="custom-row" style="display:flex;gap:.5rem;margin-bottom:.4rem;align-items:center">
      <input class="cp-label" placeholder="label" value="${esc(cp.label || "")}" style="max-width:150px" />
      <input class="cp-regex mono" placeholder="regex" value="${esc(cp.regex || "")}" style="flex:1" />
      <label style="display:flex;align-items:center;gap:.3rem;font-size:.8rem;color:var(--muted)"><input type="checkbox" class="cp-en" ${cp.enabled ? "checked" : ""} style="width:auto" />on</label>
      <button class="btn danger sm cp-del" type="button">×</button></div>`;
  const customRows = ((d.custom_patterns) || []).map(customRow).join("");

  c.innerHTML = `
    <div class="panel"><div class="panel-head"><h2>DLP policy</h2></div>
      <div style="padding:1rem 1.1rem">
        <label class="field"><span class="lab">Scan requests for secrets / tokens</span>
          <input type="checkbox" id="dlp-en" ${d.enabled ? "checked" : ""} style="width:auto" /></label>
        <label class="field"><span class="lab">Action on detection</span>
          <select id="dlp-act">${["off", "flag", "redact", "block"].map((a) =>
            `<option ${a === d.action ? "selected" : ""}>${a}</option>`).join("")}</select></label>
        <div class="lab" style="color:var(--muted);font-size:.82rem;margin:.6rem 0 .3rem">BERT NER sidecar (layer 2 — fuzzy/contextual PII)</div>
        <label class="field"><span class="lab">Use model sidecar</span>
          <input type="checkbox" id="dlp-men" ${d.model_enabled ? "checked" : ""} style="width:auto" /></label>
        <label class="field"><span class="lab">Sidecar URL</span>
          <input id="dlp-murl" value="${esc(d.model_url || "http://dlp-bert:8000")}" /></label>
        <label class="field"><span class="lab">Min score (0–1)</span>
          <input id="dlp-mscore" type="number" step="0.05" min="0" max="1" value="${d.model_min_score ?? 0.5}" /></label>
        <label class="field"><span class="lab">Sidecar URLs (one per line — overrides the single URL above)</span>
          <textarea id="dlp-murls" rows="3" class="mono" placeholder="http://dlp-bert:8000">${esc((d.model_urls || []).join("\n"))}</textarea></label>
        <label class="field"><span class="lab">Max concurrent scans per endpoint (0 = unlimited)</span>
          <input id="dlp-mconc" type="number" min="0" value="${d.model_max_concurrency ?? 0}" /></label>
        <label class="field"><span class="lab">Model scan scope</span>
          <select id="dlp-mscope">
            <option value="last_user" ${d.model_scan_scope !== "all" ? "selected" : ""}>last user message (default)</option>
            <option value="all" ${d.model_scan_scope === "all" ? "selected" : ""}>all messages</option>
          </select></label>
        <label class="field"><span class="lab">Model scan budget per request (ms)</span>
          <input id="dlp-mbudget" type="number" min="100" value="${Number(d.model_scan_budget_ms) || 2000}" /></label>
        <button class="btn" id="dlp-save">Save policy</button>
      </div>
    </div>
    <div class="panel"><div class="panel-head"><h2>Sensitive Info Detection</h2>
      <button class="btn ghost sm" id="pat-all" type="button">Enable all</button></div>
      <div style="padding:1rem 1.1rem">
        <div class="lab" style="color:var(--muted);font-size:.82rem;margin-bottom:.6rem">Choose which patterns are detected. Secrets are on by default; PII is opt-in. Model patterns need the BERT sidecar enabled above.</div>
        ${patGroups}
        <div class="lab" style="color:var(--muted);font-size:.78rem;text-transform:uppercase;margin:.9rem 0 .35rem">Custom patterns (regex)</div>
        <div id="custom-rows">${customRows}</div>
        <button class="btn ghost sm" id="add-custom" type="button" style="margin-top:.3rem">Add pattern</button>
        <div style="margin-top:1rem"><button class="btn" id="pat-save">Save detection</button></div>
      </div>
    </div>
    <div class="panel"><div class="panel-head"><h2>Capture &amp; flywheel</h2></div>
      <div style="padding:1rem 1.1rem">
        <div class="lab" style="color:var(--muted);font-size:.82rem;margin-bottom:.6rem">Records prompts (redacted by default) for audit and DLP model training. Off by default.</div>
        <label class="field"><span class="lab">Enable capture</span>
          <input type="checkbox" id="cap-en" ${cap.enabled ? "checked" : ""} style="width:auto" /></label>
        <label class="field"><span class="lab">Sample rate (0–1, incidents always captured)</span>
          <input id="cap-rate" type="number" step="0.05" min="0" max="1" value="${cap.sample_rate ?? 0}" /></label>
        <label class="field"><span class="lab">Redact stored bodies</span>
          <input type="checkbox" id="cap-red" ${cap.redact ? "checked" : ""} style="width:auto" /></label>
        <label class="field"><span class="lab">Retention (days)</span>
          <input id="cap-ret" type="number" min="1" value="${cap.retention_days ?? 30}" /></label>
        <div class="lab" style="color:var(--muted);font-size:.82rem;margin:.6rem 0 .3rem">Raw training window — stores a short-lived un-redacted copy so the flywheel scans aligned text. Stores real secrets (encrypted) until the TTL.</div>
        <label class="field"><span class="lab">Enable raw training window</span>
          <input type="checkbox" id="cap-raw" ${cap.raw_training ? "checked" : ""} style="width:auto" /></label>
        <label class="field"><span class="lab">Raw copy TTL (hours)</span>
          <input id="cap-rawttl" type="number" min="1" value="${cap.raw_ttl_hours ?? 24}" /></label>
        <button class="btn" id="cap-save">Save capture</button>
      </div>
    </div>
    <div class="row" style="margin-bottom:1rem"><button class="btn sm" id="new-hook">New alert webhook</button></div>
    ${panelTable("Alert webhooks", ["Name", "URL", "Events", "Secret", "Enabled", ""],
      hooks.map((h) => `<tr><td>${esc(h.name) || "—"}</td><td class="mono">${esc(h.url)}</td>
        <td>${(h.events || []).join(", ")}</td>
        <td>${h.has_secret ? `<span class="badge active">set</span>` : `<span class="badge neutral">none</span>`}</td>
        <td>${h.enabled ? "yes" : "no"}</td>
        <td style="text-align:right"><button class="btn danger sm" data-delhook="${h.id}">Delete</button></td></tr>`))}
    ${panelTable("Recent incidents", ["Time", "User", "Ingress", "Model", "Action", "Labels", "Matches", "Sample"],
      incidents.map((i) => `<tr><td>${fmtTime(i.ts)}</td><td>${esc(i.user) || "—"}</td><td>${esc(i.ingress)}</td>
        <td class="mono">${esc(i.alias)}</td>
        <td><span class="badge ${actBadge(i.action)}">${esc(i.action)}</span></td>
        <td>${(i.labels || []).map(esc).join(", ")}</td><td>${i.match_count}</td>
        <td class="mono" style="max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(i.sample)}</td></tr>`))}
  `;
  // PUT /api/admin/dlp replaces the whole config, so every save gathers ALL
  // fields on screen (policy + patterns + custom) to avoid clobbering the other
  // panel's settings.
  const gatherDLP = () => {
    const body = {
      enabled: $("#dlp-en").checked, action: $("#dlp-act").value, scan_responses: false,
      model_enabled: $("#dlp-men").checked, model_url: $("#dlp-murl").value.trim(),
      model_min_score: Number($("#dlp-mscore").value) || 0,
      model_urls: $("#dlp-murls").value.split("\n").map((s) => s.trim()).filter(Boolean),
      model_max_concurrency: Number($("#dlp-mconc").value) || 0,
      model_scan_scope: $("#dlp-mscope").value, model_scan_budget_ms: Number($("#dlp-mbudget").value) || 2000,
    };
    // Only send patterns/custom when the panel actually rendered, so a failed
    // catalog fetch can't silently wipe the operator's toggles on save.
    const togs = document.querySelectorAll(".pat-tog");
    if (togs.length) {
      const patterns = {};
      togs.forEach((t) => { patterns[t.getAttribute("data-label")] = t.checked; });
      body.patterns = patterns;
      const custom_patterns = [];
      document.querySelectorAll(".custom-row").forEach((r) => {
        const label = r.querySelector(".cp-label").value.trim();
        const regex = r.querySelector(".cp-regex").value;
        if (label || regex) custom_patterns.push({ label, regex, enabled: r.querySelector(".cp-en").checked });
      });
      body.custom_patterns = custom_patterns;
    }
    return body;
  };
  const saveDLP = async (okMsg) => {
    const x = await api("PUT", "/api/admin/dlp", gatherDLP());
    if (x.ok) toast(okMsg);
    else toast((x.data && x.data.error) || "Failed", "err");
  };
  $("#dlp-save").addEventListener("click", () => saveDLP("DLP policy saved"));
  $("#pat-save").addEventListener("click", () => saveDLP("Detection settings saved"));
  $("#pat-all").addEventListener("click", () => {
    document.querySelectorAll(".pat-tog").forEach((t) => { t.checked = true; });
  });
  const wireDel = (btn) => btn.addEventListener("click", () => btn.closest(".custom-row").remove());
  document.querySelectorAll(".cp-del").forEach(wireDel);
  $("#add-custom").addEventListener("click", () => {
    const wrap = document.createElement("div");
    wrap.innerHTML = `<div class="custom-row" style="display:flex;gap:.5rem;margin-bottom:.4rem;align-items:center">
      <input class="cp-label" placeholder="label" style="max-width:150px" />
      <input class="cp-regex mono" placeholder="regex" style="flex:1" />
      <label style="display:flex;align-items:center;gap:.3rem;font-size:.8rem;color:var(--muted)"><input type="checkbox" class="cp-en" checked style="width:auto" />on</label>
      <button class="btn danger sm cp-del" type="button">×</button></div>`;
    const row = wrap.firstElementChild;
    $("#custom-rows").appendChild(row);
    wireDel(row.querySelector(".cp-del"));
  });
  $("#cap-save").addEventListener("click", async () => {
    const x = await api("PUT", "/api/admin/capture", {
      enabled: $("#cap-en").checked,
      sample_rate: Number($("#cap-rate").value) || 0,
      redact: $("#cap-red").checked,
      retention_days: Number($("#cap-ret").value) || 30,
      raw_training: $("#cap-raw").checked,
      raw_ttl_hours: Number($("#cap-rawttl").value) || 24,
    });
    if (x.ok) toast("Capture config saved");
    else toast((x.data && x.data.error) || "Failed", "err");
  });
  $("#new-hook").addEventListener("click", () => editWebhook(c));
  document.querySelectorAll("[data-delhook]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm("Delete this webhook?")) return;
    const x = await api("DELETE", `/api/admin/webhooks/${b.getAttribute("data-delhook")}`);
    if (x.ok) { toast("Deleted"); adminDLP(c); } else toast("Failed", "err");
  }));
}

function editWebhook(c) {
  modalForm("New alert webhook", [
    { name: "name", label: "Name", value: "" },
    { name: "url", label: "URL", value: "" },
    { name: "secret", label: "Signing secret (HMAC, optional)", type: "password", value: "" },
    { name: "enabled", label: "Enabled", type: "checkbox", value: true },
  ], async (v) => {
    if (!v.url) { toast("URL is required", "err"); return false; }
    const x = await api("POST", "/api/admin/webhooks", {
      name: v.name, url: v.url, secret: v.secret, enabled: v.enabled, events: ["dlp.incident"],
    });
    if (x.ok) { toast("Webhook created"); adminDLP(c); return true; }
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
    <td>${esc(p.provider)}</td><td>${p.requests}</td>
    <td class="mono">${tok(p.tokens_in)} / ${tok(p.tokens_out)}</td>
    <td>$${(+p.cost_usd).toFixed(4)}</td><td>${p.p95_ms} ms</td><td>${p.errors}</td></tr>`);
  const modelRows = (d.models || []).map((m) => `<tr>
    <td class="mono">${esc(m.alias)}</td><td>${esc(m.provider)}</td><td class="mono">${esc(m.upstream_model)}</td>
    <td>${m.requests}</td><td class="mono">${tok(m.tokens_in)} / ${tok(m.tokens_out)}</td>
    <td>$${(+m.cost_usd).toFixed(4)}</td>
    <td>${m.p95_ms} ms</td><td>${m.errors}</td></tr>`);
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
    <table><thead><tr>${cols.map((c) => `<th>${esc(c)}</th>`).join("")}</tr></thead>
    <tbody>${rowsHtml.join("")}</tbody></table>`}
  </div>`;
}

function modalForm(title, fields, onSubmit) {
  const bg = document.createElement("div");
  bg.className = "modal-bg";
  bg.innerHTML = `<div class="modal"><h3>${esc(title)}</h3><form id="mf">
    ${fields.map((f) => {
      if (f.type === "checkbox") {
        return `<label class="field"><span class="lab">${esc(f.label)}</span>
          <input type="checkbox" name="${f.name}" ${f.value ? "checked" : ""} style="width:auto" /></label>`;
      }
      if (f.type === "textarea") {
        return `<label class="field"><span class="lab">${esc(f.label)}</span>
          <textarea name="${f.name}">${esc(f.value)}</textarea></label>`;
      }
      if (f.type === "select") {
        return `<label class="field"><span class="lab">${esc(f.label)}</span>
          <select name="${f.name}" ${f.disabled ? "disabled" : ""}>${(f.options || []).map((o) =>
            `<option value="${esc(o)}" ${o === f.value ? "selected" : ""}>${esc(o)}</option>`).join("")}</select></label>`;
      }
      if (f.type === "password") {
        return `<label class="field"><span class="lab">${esc(f.label)}</span>
          <input type="password" name="${f.name}" value="${esc(f.value)}" autocomplete="new-password"
            placeholder="${esc(f.placeholder || "")}" /></label>`;
      }
      return `<label class="field"><span class="lab">${esc(f.label)}</span>
        <input name="${f.name}" value="${esc(f.value)}" ${f.disabled ? "disabled" : ""} /></label>`;
    }).join("")}
    <div class="row" style="justify-content:flex-end;margin-top:.5rem">
      <button type="button" class="btn ghost" id="mf-cancel">Cancel</button>
      <button type="submit" class="btn">Save</button>
    </div></form></div>`;
  document.body.appendChild(bg);
  const close = () => bg.remove();
  $("#mf-cancel", bg).addEventListener("click", close);
  bg.addEventListener("click", (e) => { if (e.target === bg) close(); });
  $("#mf", bg).addEventListener("submit", async (e) => {
    e.preventDefault();
    const v = {};
    fields.forEach((f) => {
      const el = e.target[f.name];
      v[f.name] = f.type === "checkbox" ? el.checked : el.value;
    });
    const ok = await onSubmit(v);
    if (ok !== false) close();
  });
}

init();
