# Sensitive Info Detection (guardrails) — design

**Goal:** Reach (and slightly exceed) OpenRouter-style "Sensitive Info Detection":
a configurable set of toggleable detection patterns plus operator-defined custom
patterns, layered on the gateway's existing DLP enforcement.

## What exists vs. what this adds

The DLP layer already has: deterministic secret/credential rules + entropy, an
optional BERT-NER model layer, enforcement actions (`off`/`flag`/`redact`/`block`),
incidents, and HMAC alert webhooks. This change adds **named PII patterns**,
**per-pattern toggles**, and **custom patterns** — it does not change enforcement,
incidents, or webhooks.

## Detection patterns

A built-in pattern has a `label`, a `category` (`secret` | `pii`), a regex, an
optional post-match `validate` func, and a `defaultOn` flag.

| Label | Category | Default | Notes |
|-------|----------|---------|-------|
| existing secret rules (`openai_key`, `jwt`, `private_key`, …) | secret | on | unchanged |
| `high_entropy` | secret | on | the entropy heuristic, now toggleable |
| `email` | pii | off | RFC-ish address |
| `phone` | pii | off | E.164 / common US/intl forms |
| `ssn` | pii | off | US `NNN-NN-NNNN` (dashed, word-bounded) |
| `credit_card` | pii | off | digit run + separators, **Luhn-validated** |
| `ip_address` | pii | off | IPv4 with octet-range validation |
| `person_name` | pii (model) | off | BERT `pii:PER` — *adds latency* |
| `address` | pii (model) | off | BERT `pii:LOC` — *adds latency* |
| `organization` | pii (model) | off | BERT `pii:ORG` — *adds latency* |

Regex detection uses Go's RE2 engine, which is linear-time — operator-supplied
custom patterns cannot cause catastrophic backtracking (no ReDoS).

## Custom patterns

`{ label, regex, enabled }`. On save the regex must compile, be ≤ 512 chars, and
the set ≤ 50 entries; invalid sets are rejected (not silently dropped). Enabled
custom patterns scan alongside the built-ins.

## Config schema (`dlpConfig`, hot-reloaded)

Two new fields on the existing DLP settings:

- `patterns: map[string]bool` — built-in label → enabled. A label absent from the
  map falls back to its `defaultOn`, so partial/legacy configs keep working.
- `custom_patterns: [{label, regex, enabled}]`.

## Detection API (`internal/dlp`)

- `ScanWith(s, PatternSet) []Finding` — the toggle-aware scan; `PatternSet` carries
  the enabled map, compiled custom patterns, and an entropy flag.
- `Scan(s)` stays as `ScanWith(s, {Entropy:true})` (defaults) for back-compat
  (second-pass, existing tests).
- `BuiltinPatterns() []PatternInfo` — `{label, category, defaultOn}` for the
  config validator and the UI.
- Model findings (`pii:PER/ORG/LOC`) are filtered in `dlpEnforce` by the
  `person_name`/`address`/`organization` toggles.

## UI

A "Sensitive Info Detection" panel in the admin DLP tab: grouped toggles
(Secrets / PII / Model — model items labelled *adds latency*), an "Enable all"
control, and a custom-pattern editor (label + regex + enable + remove). Saves via
the existing `PUT /api/admin/dlp`.

## Testing

- Per PII pattern: true-positive + false-positive (Luhn rejects bad cards; octet
  validation rejects `999.999.999.999`; SSN/phone boundaries).
- `ScanWith` honours the enabled map and custom patterns; a disabled pattern
  produces no finding.
- Config validation rejects an uncompilable / over-long / over-count custom set.
- `dlpEnforce` filters model findings by the model toggles.
