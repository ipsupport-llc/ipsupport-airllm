-- Capture index: one metadata row per captured request. Bodies are stored in
-- the blob store (encrypted), never here. Keys reference capture blob objects.

CREATE TABLE capture_index (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ts                  timestamptz NOT NULL DEFAULT now(),
    key_id              uuid NULL REFERENCES api_keys(id) ON DELETE SET NULL,
    user_id             uuid NULL REFERENCES users(id) ON DELETE SET NULL,
    ingress_protocol    text NOT NULL DEFAULT '',
    alias               text NOT NULL DEFAULT '',
    provider_name       text NOT NULL DEFAULT '',
    upstream_model      text NOT NULL DEFAULT '',
    status              int NOT NULL DEFAULT 0,
    prompt_tokens       bigint NOT NULL DEFAULT 0,
    completion_tokens   bigint NOT NULL DEFAULT 0,
    cost_usd            numeric(12,6) NOT NULL DEFAULT 0,
    blob_key            text NOT NULL DEFAULT '',
    redacted            bool NOT NULL DEFAULT true,
    model_version       text NOT NULL DEFAULT '',
    detected            jsonb NOT NULL DEFAULT '[]',
    review_status       text NOT NULL DEFAULT 'unreviewed',
    secondpass_status   text NOT NULL DEFAULT 'pending',
    secondpass_labels   jsonb NOT NULL DEFAULT '[]'
);

CREATE INDEX idx_capture_index_ts ON capture_index(ts DESC);
CREATE INDEX idx_capture_index_review_status ON capture_index(review_status);
CREATE INDEX idx_capture_index_secondpass_status ON capture_index(secondpass_status);

-- Track which DLP model version produced a detection for eval/flywheel.
ALTER TABLE dlp_incidents ADD COLUMN IF NOT EXISTS model_version text NOT NULL DEFAULT '';
