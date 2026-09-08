-- Some vendors charge a second, higher pair of rates once the prompt crosses a
-- context threshold. Gemini 2.5 Pro is $1.25 / $10.00 per 1M tokens for a
-- prompt up to 200 000 tokens and $2.50 / $15.00 above it — exactly double, on
-- input and output both. The table held one rate per model, so every long
-- prompt was billed at half what it cost, and the rolling cost cap on the key
-- let through twice the spend the operator asked for.
--
-- One row per model still, carrying the breakpoint and the second pair of
-- rates, because that is both the shape the vendor publishes and the shape the
-- console already presents. The alternative — a tiered model being two rows the
-- lookup chooses between — needs no schema change but invents a model-name
-- convention every operator would have to learn.
--
-- context_threshold is a prompt-token count, and 0 means "no tier": the two
-- _above rates are then never read. 0 cannot mean anything else, since every
-- prompt is above zero.
--
-- Additive with defaults, so it cannot disturb the rows already stored: they
-- read threshold 0 and price exactly as they do today.
ALTER TABLE pricing
    ADD COLUMN context_threshold   bigint        NOT NULL DEFAULT 0,
    ADD COLUMN input_per_1m_above  numeric(12,4) NOT NULL DEFAULT 0,
    ADD COLUMN output_per_1m_above numeric(12,4) NOT NULL DEFAULT 0;

-- The invariant belongs here and not only in the admin API, the way `unit`'s
-- vocabulary does (0011). These Vertex rows are hand-entered, and a hand
-- reaches for SQL as readily as for the console: a threshold with a missing
-- rate above it would load happily and bill a long prompt at $0 — worse than
-- the flat rate it replaced. A threshold is also meaningless on an audio or
-- TTS row, which is priced by duration or characters and never by prompt
-- tokens, so storing one would be a setting that silently does nothing.
ALTER TABLE pricing ADD CONSTRAINT pricing_context_tier_complete CHECK (
    context_threshold >= 0 AND (
        context_threshold = 0
        OR (unit = 'tokens' AND input_per_1m_above > 0 AND output_per_1m_above > 0)
    )
);
