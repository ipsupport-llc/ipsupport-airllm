-- Initial schema for ipsupport-airllm.

-- Identities. In prod these come from OIDC (subject = OIDC sub); in the
-- local mock a single dev user is seeded.
CREATE TABLE users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject    text UNIQUE NOT NULL,
    email      text NOT NULL DEFAULT '',
    display    text NOT NULL DEFAULT '',
    roles      text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Role -> policy: allowed models/aliases, default per-window limits, and
-- whether the role may address providers explicitly (passthrough).
CREATE TABLE roles_policy (
    role              text PRIMARY KEY,
    allowed_models    text[] NOT NULL DEFAULT '{}',
    allow_passthrough boolean NOT NULL DEFAULT false,
    limits            jsonb NOT NULL DEFAULT '{}',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- API keys: hashed credentials owned by a user, carrying a snapshot of
-- the owner's policy taken at issue time.
CREATE TABLE api_keys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL DEFAULT '',
    hash            text UNIQUE NOT NULL,
    prefix          text NOT NULL,
    last4           text NOT NULL,
    policy_snapshot jsonb NOT NULL DEFAULT '{}',
    status          text NOT NULL DEFAULT 'active', -- active | revoked
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz
);
CREATE INDEX idx_api_keys_user ON api_keys(user_id);

-- Upstream providers and their (encrypted) credentials.
CREATE TABLE providers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text UNIQUE NOT NULL,
    kind       text NOT NULL,           -- openai | openrouter | xai | anthropic | mock
    base_url   text NOT NULL DEFAULT '',
    cred_enc   bytea,                   -- AES-GCM ciphertext (NULL for mock)
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Client-facing model aliases and their ordered upstream targets.
CREATE TABLE model_aliases (
    alias      text PRIMARY KEY,
    protocol   text NOT NULL,           -- preferred client protocol: openai | anthropic
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE alias_targets (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    alias             text NOT NULL REFERENCES model_aliases(alias) ON DELETE CASCADE,
    priority          int NOT NULL DEFAULT 0,   -- lower is tried first
    provider_name     text NOT NULL REFERENCES providers(name) ON DELETE CASCADE,
    upstream_model    text NOT NULL,
    upstream_protocol text NOT NULL             -- openai | anthropic
);
CREATE INDEX idx_alias_targets_alias ON alias_targets(alias, priority);

-- Pricing for cost metering: USD per 1M tokens.
CREATE TABLE pricing (
    model         text PRIMARY KEY,
    input_per_1m  numeric(12,4) NOT NULL DEFAULT 0,
    output_per_1m numeric(12,4) NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- One row per gateway request: the durable source of truth for usage.
CREATE TABLE usage_ledger (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ts                timestamptz NOT NULL DEFAULT now(),
    key_id            uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    user_id           uuid REFERENCES users(id) ON DELETE SET NULL,
    alias             text NOT NULL DEFAULT '',
    provider_name     text NOT NULL DEFAULT '',
    upstream_model    text NOT NULL DEFAULT '',
    ingress_protocol  text NOT NULL DEFAULT '',
    upstream_protocol text NOT NULL DEFAULT '',
    prompt_tokens     bigint NOT NULL DEFAULT 0,
    completion_tokens bigint NOT NULL DEFAULT 0,
    cost_usd          numeric(12,6) NOT NULL DEFAULT 0,
    status            int NOT NULL DEFAULT 0,
    latency_ms        bigint NOT NULL DEFAULT 0,
    error             text NOT NULL DEFAULT ''
);
CREATE INDEX idx_usage_ledger_key_ts ON usage_ledger(key_id, ts);
CREATE INDEX idx_usage_ledger_user_ts ON usage_ledger(user_id, ts);

-- Control-plane mutation audit.
CREATE TABLE audit_log (
    id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ts     timestamptz NOT NULL DEFAULT now(),
    actor  text NOT NULL DEFAULT '',
    action text NOT NULL DEFAULT '',
    target text NOT NULL DEFAULT '',
    detail jsonb NOT NULL DEFAULT '{}'
);
