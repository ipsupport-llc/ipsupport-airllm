# Auth foundation (P1) — design

**Goal:** Replace the dev-only random-per-boot mock authenticator with
production-grade control-plane auth: a full **local** mode (persistent users,
admin-managed CRUD, self-service password change, stable sessions across
restarts) **and** a full **generic OIDC** mode. Both are selectable per deploy;
the rest of the app stays auth-method-agnostic.

This is sub-project P1 of the "AirLLM as a deployable product" program
(P1 auth · P2 observability · P3 BERT-scale · P4 standalone packaging ·
P5 Helm/ArgoCD). Public-clean is a program-wide rule: **no hostnames, IPs, IDs,
or secrets in the repo** — everything environment-specific is a parameter
supplied at deploy time.

## Background

Today (`internal/auth/auth.go`) `Mock` holds three in-memory users with random
passwords regenerated every boot, and a random HMAC signing key regenerated
every boot — so the admin password changes on each restart and all sessions die
on restart. `AUTH_MODE=oidc` is accepted by config but **not implemented**
(`cmd/.../main.go` only wires `mock`). The `users` table
(`migrations/0001_init.sql`) has `subject/email/display/roles` but no password.
The `LoginProvider` / `Authenticator` / `Principal` abstraction and the HMAC
session sign/verify already exist and are reused unchanged.

## Modes

`AUTH_MODE`:
- `local` — DB-backed username/password. The full standalone product.
- `oidc` — generic OpenID Connect (any compliant IdP).
- `mock` — **deprecated alias** for `local` (so existing configs/compose keep
  working); logged as deprecated at startup.

`config.Load` accepts `local | oidc | mock`; `mock` is normalized to `local`.

## Stable session key

The HMAC session signing key must be stable so sessions survive restarts and so
multiple replicas (k8s) accept each other's cookies. Source, in order:
1. `AIRLLM_SESSION_KEY` (base64, 32 bytes) if set — explicit override.
2. Otherwise derive deterministically: `HKDF-SHA256(masterKey, info="airllm-session-v1", 32)`.

The master key is already required in prod and has a deterministic dev fallback,
so this yields a stable key in every environment with **no new secret to
manage**. `config.Config` gains `SessionKey []byte`. Both `LocalAuth` and the
OIDC handler sign cookies with it.

## Schema (migration `0007_local_auth.sql`)

```sql
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash   text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_set_at timestamptz NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled        bool NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_source     text NOT NULL DEFAULT 'local';
```

`auth_source` distinguishes locally-managed users (`local`) from IdP-provisioned
ones (`oidc`). Passwords are **bcrypt** hashes (`golang.org/x/crypto/bcrypt`,
default cost). `password_hash = ''` means "no local password" (e.g. an
OIDC-provisioned user) and can never satisfy a local login.

## Local auth (`AUTH_MODE=local`)

New `internal/auth/local.go` — `LocalAuth` implements `LoginProvider` +
`Authenticator`, backed by a small store interface:

```go
type UserStore interface {
    ByUsername(ctx, username string) (UserRow, error) // subject OR email match
    // CRUD used by the admin handlers (see API)
}
type UserRow struct {
    ID, Subject, Email, Display string
    Roles []string
    PasswordHash string
    Disabled bool
    AuthSource string
}
```

- `Login(username, password)`: look up by username (subject, case-insensitive) or
  email; reject if `disabled` or `password_hash == ""`; `bcrypt.CompareHashAndPassword`;
  on success return `Principal{Subject, Email, Roles}`. A dummy bcrypt compare runs
  on "user not found" to avoid a timing oracle.
- `SetSession/ClearSession/Authenticate`: reuse the existing HMAC cookie code,
  keyed by the stable session key.

### Bootstrap admin (idempotent)

On startup in `local` mode, `EnsureBootstrapAdmin`:
1. If any non-disabled user with `airllm_admin` exists → no-op.
2. Else create user `admin` (`auth_source=local`, role `airllm_admin`) with:
   - password from `AIRLLM_ADMIN_PASSWORD` if set, else a random token **logged
     once** at WARN (the only time it is ever logged), and
   - bcrypt hash persisted.

So the admin password is permanent until changed — it is never regenerated on
restart. `AIRLLM_ADMIN_USERNAME` (default `admin`) optionally overrides the name.

### Dev demo seed

`ENV=dev` continues to seed sample `operator` (airllm_user) and `auditor`
(airllm_auditor) users so the console click-through works — now **persisted**
with bcrypt hashes; their generated passwords are logged once. Production seeds
only the bootstrap admin.

## User management (admin) + self password change

All admin routes are `airllm_admin`-gated and audited.

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/admin/users` | List users (incl. `disabled`, `auth_source`) — extend existing |
| `POST` | `/api/admin/users` | Create local user `{username, email, display, roles, password}` |
| `PUT` | `/api/admin/users/{id}` | Update `email/display/roles/disabled` |
| `POST` | `/api/admin/users/{id}/password` | Admin resets a user's password |
| `DELETE` | `/api/admin/users/{id}` | Delete a user — blocked if they still own active API keys (revoke first); `Disable` is the non-destructive alternative |
| `POST` | `/api/me/password` | **Self**: change own password (requires current password); local-auth users only |

Validation: username non-empty + unique; roles must be known role keys; password
length floor (≥ 8). An admin cannot disable or delete the last remaining admin
(guard against lock-out). Creating/editing a user with `auth_source=oidc` cannot
set a local password (so `/api/me/password` and admin reset are no-ops for them).

UI (`web/static/app.js`):
- Admin **users** tab → create / edit (roles, disable) / reset-password / delete.
- A "Change password" affordance in the sidebar footer for the logged-in user.
- Login screen gains a "Sign in with SSO" button shown only in `oidc` mode
  (`/api/auth/mode` exposes the active mode + SSO URL).

### Disabled semantics

Disabling blocks new logins immediately. An existing session cookie remains valid
until its 12h TTL expires (stateless HMAC; no per-request DB hit). The user's API
keys keep working until revoked — disabling a user does **not** auto-revoke keys
(admin revokes via the existing keys UI). Documented behavior, not a gap.

## OIDC (`AUTH_MODE=oidc`)

New `internal/auth/oidc.go` — `OIDCAuth` implements `Authenticator` plus login
start/callback handlers. Libraries: `github.com/coreos/go-oidc/v3/oidc`
(discovery + JWKS + ID-token verification) and `golang.org/x/oauth2`
(authorization-code exchange + PKCE).

### Config (all env, no repo hardcoding)

| Var | Meaning |
|-----|---------|
| `OIDC_ISSUER` | Issuer URL (discovery at `/.well-known/openid-configuration`) |
| `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` | Relying-party credentials |
| `OIDC_REDIRECT_URL` | Callback URL, e.g. `https://airllm.example.com/auth/callback` |
| `OIDC_SCOPES` | Default `openid profile email` |
| `OIDC_ROLES_CLAIM` | Claim holding roles; supports a string array **or** an object whose keys are roles (Zitadel `urn:zitadel:iam:org:project:roles`) |
| `OIDC_ROLE_MAP` | Optional `idpRole:airllmRole,...`; default identity map onto `airllm_admin/user/auditor` |

In `oidc` mode the OIDC vars are required; `config.Load` validates them.

### Flow

Distinct routes avoid clashing with the local `POST /auth/login`: the OIDC start
is `GET /auth/sso`, the callback `GET /auth/callback`.

1. `GET /auth/sso` → build the authorize URL with PKCE (`code_challenge`),
   `state`, and `nonce`; store `state`/`nonce`/`code_verifier` in short-lived
   signed cookies; 302 to the IdP.
2. `GET /auth/callback` → validate `state`; exchange the code at the token
   endpoint (with `code_verifier`); verify the ID token (signature via JWKS,
   `iss`, `aud`, `exp`, `nonce`); read `sub`, `email`, and roles via
   `OIDC_ROLES_CLAIM`; apply `OIDC_ROLE_MAP`; **upsert** the user
   (`auth_source=oidc`, roles refreshed from the token); set our HMAC session
   cookie; 302 to `/`.
3. The rest of the app uses the same `Authenticator`/session — auth-method-agnostic.

### Session model

Stateless HMAC cookie (12h), re-SSO on expiry. **No** server-side session store
and **no** OIDC refresh-token persistence in P1. Silent refresh (which needs a
sessions table) is explicitly out of scope for P1.

## Wiring (`cmd/ipsupport-airllm/main.go`)

Select the provider by `AUTH_MODE`:
- `local`: build `LocalAuth` (session key + user store), run `EnsureBootstrapAdmin`,
  set `deps.Auth`/`deps.Login`.
- `oidc`: build `OIDCAuth` (discovery + verifier + oauth2 config), set
  `deps.Auth`; register `GET /auth/sso` + `GET /auth/callback`; `deps.Login` is
  nil (no password login).

`internal/httpapi` already treats `Login` as nil-able (OIDC). Login/logout and
the new `/api/auth/mode` endpoint report the active mode so the UI renders the
right screen.

## Components / files

- `internal/config/config.go` — `AUTH_MODE` (local|oidc|mock-alias), `SessionKey`
  (HKDF or override), OIDC vars + validation.
- `internal/auth/local.go` — `LocalAuth`, bcrypt, bootstrap admin.
- `internal/auth/oidc.go` — `OIDCAuth`, login/callback, roles mapping.
- `internal/auth/session.go` — extract the shared HMAC sign/verify out of `Mock`
  so both providers reuse it (keyed by `SessionKey`).
- `internal/store/users.go` — user CRUD queries (`ByUsername`, `Create`, `Update`,
  `SetPassword`, `Disable`, `Delete`, `List`, `CountAdmins`).
- `internal/httpapi/api_users.go` — admin user CRUD + `/api/me/password` +
  `/api/auth/mode`.
- `migrations/0007_local_auth.sql`.
- `internal/seed/seed.go` — persist dev demo users with hashes.
- `web/static/app.js` — users-tab CRUD, change-password, SSO login button.

## Testing

- **bcrypt**: hash/verify round-trip; wrong password rejected; empty hash never
  matches.
- **Stable session key**: HKDF determinism; a cookie signed by one instance
  verifies on a second instance built from the same master key; an override key
  is honored.
- **Bootstrap admin**: creates on empty; idempotent when an admin exists; uses
  `AIRLLM_ADMIN_PASSWORD` when set; never logs an env-provided password.
- **Local login**: disabled user rejected; unknown user rejected (constant-time);
  roles come from the DB.
- **User CRUD**: create/update/disable/delete; last-admin guard; role validation;
  self password-change requires the current password.
- **OIDC** (unit, mock IdP via `httptest`): a signed ID token from a test issuer
  verifies; roles parsed from both array and object-keyed claims; `OIDC_ROLE_MAP`
  applied; expired/invalid-nonce/bad-signature tokens rejected. Full e2e against a
  real Zitadel happens in the k8s-deploy sub-project.

## Security

- bcrypt (default cost); password floor ≥ 8; reset/admin-set vs self-change
  (current-password required) separated.
- Session key stable but secret; never logged. Generated bootstrap password
  logged exactly once; env-provided password never logged.
- PKCE + `state` + `nonce` on the OIDC flow; ID-token signature/iss/aud/exp/nonce
  all verified; cookies `HttpOnly`, `SameSite=Lax`.
- Last-admin lock-out guard.

## Out of scope (P1)

- Server-side session store / OIDC silent refresh (re-SSO on expiry instead).
- Full OIDC e2e against the live Zitadel (k8s-deploy sub-project).
- SCIM / IdP user provisioning sync (users are upserted lazily on first login).
- Simultaneous local + OIDC ("break-glass admin alongside SSO") — single mode per
  deploy in P1.

## Public-clean

No issuer URLs, client IDs/secrets, redirect hosts, or passwords in the repo —
all are env vars documented with placeholders in `.env.example` and the Helm
`values.yaml` (delivered in P4/P5). The repo ships only defaults and placeholders.
