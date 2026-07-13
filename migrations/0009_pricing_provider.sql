-- Pricing becomes provider-scoped. provider = '' is a wildcard row matching
-- any provider; all pre-existing rows become wildcards (upgrade-neutral).
ALTER TABLE pricing ADD COLUMN provider text NOT NULL DEFAULT '';
ALTER TABLE pricing DROP CONSTRAINT pricing_pkey;
ALTER TABLE pricing ADD PRIMARY KEY (provider, model);
