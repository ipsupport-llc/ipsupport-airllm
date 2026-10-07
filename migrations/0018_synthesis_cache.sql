-- Synthesis cache: a speech request an alias has answered before is served
-- from the clip the serving tier rendered (the clips themselves live in
-- Redis).
--
-- synthesis_cache switches it on per alias; off, which every existing alias
-- is, every speech request reaches a provider. synthesis_cache_ttl_s is how
-- long a clip is kept; 0 means the gateway default.
ALTER TABLE model_aliases
    ADD COLUMN synthesis_cache boolean NOT NULL DEFAULT false,
    ADD COLUMN synthesis_cache_ttl_s int NOT NULL DEFAULT 0 CHECK (synthesis_cache_ttl_s >= 0);

-- cached marks a speech request answered from the synthesis cache: no
-- provider was called for it, so it costs nothing, and attempts counts only
-- the tiers that failed before the cached clip was found.
ALTER TABLE usage_ledger
    ADD COLUMN cached boolean NOT NULL DEFAULT false;
