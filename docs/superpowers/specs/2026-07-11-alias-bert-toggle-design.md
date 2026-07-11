# Per-Alias BERT Scan Toggle — Design

**Date:** 2026-07-11
**Status:** approved

## Problem

The layer-2 BERT DLP scan is global: every chat on every alias pays the
sidecar round-trip (and its latency) even where fuzzy-PII scanning makes no
sense (e.g. a local private model, or a code-generation alias where NER noise
is useless). The operator wants a per-alias switch: route this alias through
BERT or not.

## Scope

- The toggle controls ONLY the layer-2 model scan. Layer-1 deterministic
  scanning (secret/token regexes + entropy) always runs when DLP is enabled —
  leaking an API key is a leak regardless of the alias.
- Explicit passthrough models (`provider/model`) have no alias row: they scan
  (default-on semantics).
- Default for new and existing aliases: **on** (`true`) — current behavior is
  preserved on upgrade.

## Design

### Storage

Migration `migrations/0008_alias_dlp_model.sql`:

```sql
-- Per-alias switch for the layer-2 (BERT) DLP scan. Layer-1 deterministic
-- scanning is not affected.
ALTER TABLE model_aliases
    ADD COLUMN dlp_model_scan boolean NOT NULL DEFAULT true;
```

### Routing plan carries the flag

`routing.Plan` gains `DLPModelScan bool`. `Router.Resolve`:

- alias path: `SELECT strategy, dlp_model_scan FROM model_aliases WHERE alias
  = $1` — flag copied into the plan.
- passthrough path: `DLPModelScan: true`.

The data-plane handlers resolve the plan BEFORE `dlpEnforce` runs (both
`handleChatCompletions` and the Anthropic `handleMessages`), so the flag is
available with zero extra queries.

### Enforcement

`dlpEnforce(ctx, ak, ingress, req)` gains a `modelScan bool` parameter (the
plan's flag). Inside: `modelOn = cfg.ModelEnabled && modelScan &&
len(cfg.effectiveModelURLs()) > 0`. When gated off, no `/scan` call, no
model-skip metric (this is configuration, not a failure — the
`airllm_dlp_model_skipped_total` reasons stay reserved for `all_busy` /
`no_endpoints`).

Both ingress paths pass their plan's flag. Everything else in dlpEnforce
(layer-1, redaction, incidents, capture) is unchanged.

### Admin API

- `PUT /api/admin/aliases/{alias}`: body gains `dlp_model_scan` (optional
  bool; omitted → `true`, preserving old clients).
- `GET /api/admin/aliases`: each alias includes `dlp_model_scan`.

### Console

- Alias editor: checkbox "Layer-2 BERT scan (fuzzy PII)" — checked by
  default; sits between the strategy select and the targets block.
- Aliases table: a "BERT" column — `on` (neutral badge) / `off` (revoked-style
  badge) so the exception is visible at a glance.

## Testing

- Gated integration (TEST_DATABASE_URL, rolled-back tx): alias saved with
  `dlp_model_scan=false` round-trips through the PUT/GET SQL; `Resolve`
  returns the flag for the alias and `true` for passthrough.
- Unit: dlpEnforce with `modelScan=false` performs no model scan while
  layer-1 findings still work (existing dlp test harness patterns).
- Live (compose): alias with BERT off → dlp-bert sidecar log shows no /scan
  for its traffic; alias with BERT on → scans appear; UI checkbox round-trips.

## Out of scope

- No per-alias layer-1 toggles, no per-alias DLP action override.
- No per-target granularity (the alias is the unit).
