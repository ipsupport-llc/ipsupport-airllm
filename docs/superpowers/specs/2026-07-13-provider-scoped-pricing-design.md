# Provider-Scoped Pricing + Import from Provider — Design

**Date:** 2026-07-13
**Status:** approved

## Problem

Pricing is keyed by upstream model name alone (`pricing(model PK)`), but the
same model is served by different providers at different prices (direct
OpenAI vs OpenRouter markup vs a free tier). There is no way to express that.
And OpenRouter exposes hundreds of models — hand-typing rows is hopeless,
while its `/models` API already publishes per-token prices.

## Part 1 — pricing keyed by (provider, model)

### Schema (migration 0009)

```sql
-- Pricing becomes provider-scoped. provider = '' is a wildcard row that
-- applies to any provider (all pre-existing rows become wildcards, so
-- upgrade changes nothing).
ALTER TABLE pricing ADD COLUMN provider text NOT NULL DEFAULT '';
ALTER TABLE pricing DROP CONSTRAINT pricing_pkey;
ALTER TABLE pricing ADD PRIMARY KEY (provider, model);
```

### Lookup

`pricing.Table` becomes two-level; `CostMicroUSD(provider, model, prompt,
completion)`:

1. exact `(provider, model)` match;
2. fallback to the wildcard `("", model)`;
3. otherwise 0 (unknown = free, unchanged).

Caller: `finalizeUsage` passes `entry.ProviderName` (already on the ledger
entry). `Set(provider, model, price)` for admin updates; `Load` reads the new
column.

### API

- `GET /api/admin/pricing` — each row gains `"provider"` (`""` = any).
- `PUT /api/admin/pricing/{model}` — body gains optional `"provider"`
  (default `""`); upsert on the composite key. Old consoles keep writing
  wildcard rows — backward compatible.

### Console

Pricing tab: "Provider" column (`(any)` for wildcard); the edit modal gains a
provider select built from the providers list plus `(any)`.

## Part 2 — Import prices from the provider

OpenRouter's `GET {base}/models` items carry
`pricing.prompt`/`pricing.completion` (USD **per token**, as strings). Other
OpenAI-compatible providers return no pricing fields.

- `internal/providers`: new optional capability
  `PricedModelLister { ListModelPricing(ctx) ([]ModelPrice, error) }` with
  `ModelPrice{ID string; InputPer1M, OutputPer1M float64}` (per-token × 1e6).
  Implemented by `OpenAICompat`: parse the pricing object when present, skip
  entries without pricing. `Mock` implements it with a fixed entry (tests).
- `POST /api/admin/pricing/import/{provider}` (admin): resolve the provider
  from the registry, type-assert the capability, fetch, upsert all rows keyed
  `(provider, model)`, refresh the in-memory table, audit, and return
  `{"imported": N}`. Providers whose catalog has no pricing (plain OpenAI)
  import 0 — the response says so, not an error.
- Console: an "Import prices" button on the pricing tab with a provider
  select; toast reports the imported count.

## Testing

- Unit (`internal/pricing`): exact-beats-wildcard precedence, wildcard
  fallback, unknown = 0, Set/lookup round-trip.
- Unit (`internal/providers`): `ListModelPricing` against httptest — entries
  with pricing parsed (per-token → per-1M), entries without pricing skipped,
  non-200 → error.
- Handler (httptest + mock provider): import endpoint upserts and reports
  count; unknown provider 404; provider without capability → imported 0.
- Live (compose): stub provider emitting pricing fields → import → rows in
  the pricing tab → a chat via that provider produces non-zero `cost_usd` in
  the ledger and the usage breakdown; wildcard fallback verified by pricing a
  model only as `('', model)`.

## Out of scope

- No scheduled price refresh (manual import button only).
- No currency other than USD; no per-key/per-role price overrides.
- No pricing deletion endpoint (not present today either).
