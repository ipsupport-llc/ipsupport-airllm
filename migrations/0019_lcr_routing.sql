-- LCR-style within-tier ordering strategy: a new `lcr` value for
-- model_aliases.strategy, with two supporting columns. Both are free to
-- carry any value when strategy != 'lcr' (ignored, same pattern as
-- alias_targets.options' free-form keys) — no DB-level CHECK coupling them
-- to strategy; the admin API validates that relationship on save.
ALTER TABLE model_aliases ADD COLUMN lcr_mode text NOT NULL DEFAULT 'price';
ALTER TABLE model_aliases ADD COLUMN lcr_min_availability double precision NOT NULL DEFAULT 0.5;
