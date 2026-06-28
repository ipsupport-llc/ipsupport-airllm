-- DLP: settings, incidents, and alert webhooks.

-- Generic key/value settings (DLP config lives under name = 'dlp').
CREATE TABLE settings (
    name       text PRIMARY KEY,
    value      jsonb NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One row per detection. The secret value is never stored; only labels,
-- counts, and a redacted sample.
CREATE TABLE dlp_incidents (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ts               timestamptz NOT NULL DEFAULT now(),
    key_id           uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    user_id          uuid REFERENCES users(id) ON DELETE SET NULL,
    ingress_protocol text NOT NULL DEFAULT '',
    alias            text NOT NULL DEFAULT '',
    action           text NOT NULL DEFAULT '',   -- flagged | redacted | blocked
    labels           text[] NOT NULL DEFAULT '{}',
    match_count      int NOT NULL DEFAULT 0,
    sample           text NOT NULL DEFAULT ''     -- redacted excerpt
);
CREATE INDEX idx_dlp_incidents_ts ON dlp_incidents(ts DESC);

-- Outbound alert webhooks (HMAC-signed). The secret is a signing key, never
-- returned by the API.
CREATE TABLE webhooks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL DEFAULT '',
    url        text NOT NULL,
    secret     text NOT NULL DEFAULT '',
    events     text[] NOT NULL DEFAULT '{dlp.incident}',
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
