-- Per-target failover policy, and the usage ledger's record of it.
--
-- options is a free-form object on each alias target. The gateway reads a few
-- known keys from it (a time budget, a flag that makes upstream auth/billing
-- failures fall through to the next tier) and ignores the rest, so later
-- behaviours can add keys without another migration. The empty object means
-- "use the gateway-wide defaults", which are themselves off by default, so
-- every existing alias keeps its behaviour.
ALTER TABLE alias_targets
    ADD COLUMN options jsonb NOT NULL DEFAULT '{}';

-- tier is the configured priority of the target that served the request (or
-- of the last one attempted, on failure) — the priority value itself, not a
-- position, so it stays comparable when targets in other tiers are disabled;
-- attempts is how many upstream calls the request made. Rows written before this ship read 0 for
-- both: attempts = 0 there means "not recorded", not "no call was made".
ALTER TABLE usage_ledger
    ADD COLUMN tier int NOT NULL DEFAULT 0,
    ADD COLUMN attempts int NOT NULL DEFAULT 0;
