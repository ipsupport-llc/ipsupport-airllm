-- Concurrency caps and load-balancing strategy.

-- Max simultaneous in-flight requests per provider (0 = unlimited).
ALTER TABLE providers ADD COLUMN max_concurrency int NOT NULL DEFAULT 0;

-- Within-tier load-balancing strategy for an alias:
-- round_robin (default) | least_busy. Targets sharing a priority form a tier
-- that is load-balanced; lower-priority tiers are fallback.
ALTER TABLE model_aliases ADD COLUMN strategy text NOT NULL DEFAULT 'round_robin';
