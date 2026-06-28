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

let me = null;

// ---------- bootstrap ----------
async function init() {
  const r = await api("GET", "/api/me");
  if (r.status === 401) { renderLogin(); return; }
  if (!r.ok) { app.innerHTML = `<div class="login-wrap"><div class="login-card"><h1>AirLLM</h1><p>Backend unavailable.</p></div></div>`; return; }
  me = r.data;
  renderShell();
}

// ---------- login ----------
function renderLogin() {
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
  app.innerHTML = `
    <div class="shell">
      <aside class="sidebar">
        <div class="brand">Air<span>LLM</span></div>
        <nav class="nav">
          ${NAV.map((n) => `<a href="${n.href}" data-nav>${n.label}</a>`).join("")}
          ${adminLink}
        </nav>
        <div class="sidebar-foot">
          <div class="who">${esc(me.subject)}</div>
          <div>${(me.roles || []).join(", ") || "no roles"}</div>
          <button class="btn ghost sm" id="logout" style="margin-top:.6rem">Sign out</button>
        </div>
      </aside>
      <main class="main"><div id="view"></div></main>
    </div>`;
  $("#logout").addEventListener("click", async () => { await api("POST", "/auth/logout"); me = null; renderLogin(); });
  window.onhashchange = route;
  route();
}

function setActiveNav() {
  const hash = location.hash || "#/";
  document.querySelectorAll("[data-nav]").forEach((a) => {
    const href = a.getAttribute("href");
    const on = href === hash || (href === "#/admin/users" && hash.startsWith("#/admin"));
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
  } else if (hash === "#/keys") viewKeys(view);
  else if (hash === "#/usage") viewUsage(view);
  else viewDashboard(view);
}

// ---------- dashboard ----------
async function viewDashboard(view) {
  view.innerHTML = `<h1 class="page-title">Dashboard</h1><div id="dash"></div>`;
  const [u, k] = await Promise.all([api("GET", "/api/usage"), api("GET", "/api/keys")]);
  const usage = u.data || {};
  const keys = (k.data && k.data.keys) || [];
  const active = keys.filter((x) => x.status === "active").length;
  $("#dash").innerHTML = `
    ${usageCards(usage)}
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
  const u = await api("GET", "/api/usage");
  $("#u").innerHTML = usageCards(u.data || {}) +
    `<p class="card-sub" style="color:var(--muted)">Rolling windows, enforced per API key. Limits are configured by your role policy.</p>`;
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
  const u = await api("GET", "/api/admin/usage");
  c.innerHTML = `<h2 style="font-size:1.05rem">Organization usage</h2>` + usageCards(u.data || {});
}

async function adminUsers(c) {
  const r = await api("GET", "/api/admin/users");
  const users = (r.data && r.data.users) || [];
  c.innerHTML = panelTable("Users", ["Subject", "Email", "Roles", "Created"],
    users.map((u) => `<tr><td>${esc(u.subject)}</td><td>${esc(u.email)}</td>
      <td>${(u.roles || []).map((x) => `<span class="badge ${x === "airllm_admin" ? "admin" : "neutral"}">${esc(x)}</span>`).join(" ")}</td>
      <td>${fmtTime(u.created_at)}</td></tr>`));
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
        <td class="mono">${esc(JSON.stringify(rp.limits || {}))}</td>
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
    panelTable("Model aliases", ["Alias", "Protocol", "Strategy", "Targets", ""],
      al.map((a) => `<tr><td class="mono">${esc(a.alias)}</td><td>${esc(a.protocol)}</td>
        <td>${esc(a.strategy || "round_robin")}</td>
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

  const targets = (a.targets && a.targets.length)
    ? a.targets
    : [{ provider: providers[0], upstream_model: "", upstream_protocol: "openai" }];

  const bg = document.createElement("div");
  bg.className = "modal-bg";
  bg.innerHTML = `<div class="modal" style="max-width:560px">
    <h3>${esc(a.alias ? "Edit alias " + a.alias : "New alias")}</h3>
    <label class="field"><span class="lab">Alias (the model name clients request)</span>
      <input id="al-alias" value="${esc(a.alias || "")}" ${a.alias ? "disabled" : ""} /></label>
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
  function addRow(t) {
    const row = document.createElement("div");
    row.className = "row tgt";
    row.style.marginBottom = ".4rem";
    row.innerHTML = `
      <input class="t-prio" type="number" min="0" value="${Number(t.priority) || 0}" title="priority / tier (same number = load-balanced)" style="width:64px" />
      <select class="t-prov" style="width:auto">${provOpts(t.provider)}</select>
      <input class="t-model" placeholder="upstream model" value="${esc(t.upstream_model || "")}" style="flex:1;min-width:120px" />
      <select class="t-proto" style="width:auto">
        <option ${t.upstream_protocol !== "anthropic" ? "selected" : ""}>openai</option>
        <option ${t.upstream_protocol === "anthropic" ? "selected" : ""}>anthropic</option>
      </select>
      <button type="button" class="btn danger sm t-del" title="remove">×</button>`;
    row.querySelector(".t-del").addEventListener("click", () => row.remove());
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
    const tlist = [...tdiv.querySelectorAll(".tgt")].map((r) => ({
      priority: Number(r.querySelector(".t-prio").value) || 0,
      provider: r.querySelector(".t-prov").value,
      upstream_model: r.querySelector(".t-model").value.trim(),
      upstream_protocol: r.querySelector(".t-proto").value,
    })).filter((t) => t.upstream_model);
    if (tlist.length === 0) { toast("Add at least one target with a model", "err"); return; }
    const x = await api("PUT", `/api/admin/aliases/${encodeURIComponent(alias)}`,
      { protocol: $("#al-proto", bg).value, strategy: $("#al-strategy", bg).value, targets: tlist });
    if (x.ok) { toast("Alias saved"); close(); adminAliases(c); }
    else toast((x.data && x.data.error) || "Failed", "err");
  });
}

async function adminDLP(c) {
  const [cfgR, incR, whR] = await Promise.all([
    api("GET", "/api/admin/dlp"),
    api("GET", "/api/admin/dlp/incidents"),
    api("GET", "/api/admin/webhooks"),
  ]);
  const d = cfgR.data || { enabled: false, action: "off" };
  const incidents = (incR.data && incR.data.incidents) || [];
  const hooks = (whR.data && whR.data.webhooks) || [];
  const actBadge = (a) => a === "blocked" ? "revoked" : (a === "redacted" ? "admin" : "neutral");

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
        <button class="btn" id="dlp-save">Save policy</button>
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
  $("#dlp-save").addEventListener("click", async () => {
    const x = await api("PUT", "/api/admin/dlp", {
      enabled: $("#dlp-en").checked, action: $("#dlp-act").value, scan_responses: false,
      model_enabled: $("#dlp-men").checked, model_url: $("#dlp-murl").value.trim(),
      model_min_score: Number($("#dlp-mscore").value) || 0,
    });
    if (x.ok) toast("DLP policy saved");
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
