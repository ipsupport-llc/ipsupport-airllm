-- Call affinity: a client session that a backup tier served stays on that
-- tier for its later requests (the pins themselves live in Redis).
--
-- session_affinity switches it on per alias; off, which every existing alias
-- is, the session header changes nothing about routing. session_affinity_ttl_s
-- is how long a pin lasts after the session's last request served there; 0
-- means the gateway default.
ALTER TABLE model_aliases
    ADD COLUMN session_affinity boolean NOT NULL DEFAULT false,
    ADD COLUMN session_affinity_ttl_s int NOT NULL DEFAULT 0 CHECK (session_affinity_ttl_s >= 0);

-- session is the request's X-Session-Id header, so every request of one call
-- can be listed together; NULL when the client sent none.
ALTER TABLE usage_ledger
    ADD COLUMN session text;

CREATE INDEX idx_usage_ledger_session ON usage_ledger(session, ts) WHERE session IS NOT NULL;
