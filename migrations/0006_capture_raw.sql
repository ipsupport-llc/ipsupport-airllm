-- Raw training window. When capture.raw_training is on, a second, un-redacted
-- copy of the captured body is stored (encrypted) under raw_blob_key and
-- deleted after raw_expires_at. It lets the DLP flywheel — second-pass
-- false-positive clearing and dataset export — work on byte-aligned spans even
-- when the durable capture body is redacted. Off by default; bounded by TTL.

ALTER TABLE capture_index ADD COLUMN IF NOT EXISTS raw_blob_key   text NOT NULL DEFAULT '';
ALTER TABLE capture_index ADD COLUMN IF NOT EXISTS raw_expires_at timestamptz NULL;

CREATE INDEX IF NOT EXISTS idx_capture_index_raw_expires
    ON capture_index(raw_expires_at) WHERE raw_blob_key <> '';
