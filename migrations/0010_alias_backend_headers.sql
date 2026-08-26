-- Per-alias switch: expose which real target answered via an X-Backend-Model
-- response header. Off by default — the response body's "model" field stays
-- the alias name either way, this only ever adds a header, never the body.
--
-- The header value is an operator-chosen label per target (display_label),
-- never the real provider name or upstream model id — the operator's
-- routing setup stays private even when the header is on. A target with no
-- label set contributes nothing (no header, real names never leak by
-- accident).
ALTER TABLE model_aliases
    ADD COLUMN expose_backend_headers boolean NOT NULL DEFAULT false;
ALTER TABLE alias_targets
    ADD COLUMN display_label text NOT NULL DEFAULT '';
