# LCR-style cost/availability-aware routing — design

> Status: brainstormed and reviewed (operator + two independent local review
> passes, `codex` and `muse` CLIs — both read the real code, findings below
> are self-verified against it, not taken on faith). Not yet planned or
> implemented.

## Goal

Add a third within-tier ordering strategy to AirLLM's existing alias/routing
system, alongside the current `round_robin` and `least_busy`: order a tier's
targets by price and/or a live measured availability signal, the way a
telecom LCR (least-cost-routing) engine picks the cheapest carrier among
those that clear a quality bar. The operator has 20 years of SIP/telecom
background and explicitly wants this grounded in how real LCR/quality-routing
engines behave (SIP's own Retry-After handling was the precedent for the
sibling `internal/unavail` feature, v0.1.78; this is the same instinct
applied to target *selection* rather than *retry timing*).

## Scope (settled, not open for revision)

- This is **one more within-tier strategy**, not a replacement for the
  manual priority-tier system. Tiers (`alias_targets.priority`) remain the
  operator-set hard fallback boundary exactly as today; `internal/breaker`
  (per alias+tier) and `internal/unavail` (per provider+model) remain the
  hard admission gates that decide whether a target is tried *at all* this
  attempt. The new strategy only decides the ORDER candidates are tried in,
  same contract `round_robin`/`least_busy` already have via
  `routing.Plan.Ordered`.
- `model_aliases.strategy` gains a third value: `lcr`.

## Data model

New migration `migrations/0019_lcr_routing.sql`:

```sql
ALTER TABLE model_aliases ADD COLUMN lcr_mode text NOT NULL DEFAULT 'price';
ALTER TABLE model_aliases ADD COLUMN lcr_min_availability double precision NOT NULL DEFAULT 0.5;
```

`lcr_mode` is read only when `strategy = 'lcr'`; `lcr_min_availability` is
read only when `strategy = 'lcr' AND lcr_mode = 'both'`. Both columns are
free to carry any stored value otherwise (same "ignored unless relevant"
pattern `TargetOptions.RecognitionModels`/`Voices` already use) — no
validation coupling between `strategy` and these two columns at the DB
level, only at the admin API save path (see Validation below).

`lcr_mode` accepted values: `price`, `availability`, `both`.
`lcr_min_availability` valid range: `[0, 1]`.

## The three modes

Ordering happens over a tier's full candidate list, same as `round_robin`/
`least_busy` today — `routing.Plan.Ordered` stays a pure function with no
I/O (see Architecture below for why). Breaker/unavail admission filtering
happens exactly where it does today, AFTER ordering, inside `executePlan`'s
per-target loop.

- **`price`**: sort candidates by price ascending (cheapest first). No
  availability signal consulted at all.
- **`availability`**: sort candidates by live EWMA availability descending
  (healthiest-measured first). No price signal consulted at all.
- **`both`**: **threshold-gate, not a weighted blend.** Partition candidates
  into "at or above `lcr_min_availability`" and "below it"; the below-
  threshold group sorts after the at-or-above group (never excluded outright
  — breaker/unavail already own hard exclusion; LCR only deprioritizes).
  Within each group, sort by price ascending.

### Worked examples (target use cases, not hypothetical)

`strategy` lives on the alias, so it applies uniformly across every tier of
that alias — but a tier holding exactly one target has nothing to reorder,
so a single alias can freely mix "one pinned local target in one tier" with
"several LCR-ranked remote targets in another tier" without conflict. Two
real configurations this needs to support cleanly:

- **A voice alias** (STT/TTS): tier 0 = the local provider alone (its
  existing `providers.max_concurrency` cap, e.g. `2`, is an already-shipped,
  unrelated feature — nothing new needed there); tier 1 (fallback) = the
  remote providers, `strategy=lcr`. When both local concurrency slots are
  busy, `executePlan` already moves on to the next tier on a busy target
  (`failover.go:236-238` — "a busy [target] moves on to the next target"),
  landing on tier 1's LCR-ordered remote candidates. Local-first for
  latency/cost, LCR-ranked remote as the overflow/outage fallback.
- **A chat alias**: tier 0 = several remote providers, `strategy=lcr`
  ranking them; the LAST tier (highest `priority` number) = the local model
  alone, a pure safety net tried only once every remote tier has failed.
  Remote-first for quality/cost optimization via LCR, local-last as the
  always-available floor.

Both are pure configuration (tier composition + existing `max_concurrency`)
once `lcr` exists as a strategy value — no additional mechanism beyond what
this spec already describes.

### Why not a weighted-sum score (rejected during design)

An earlier draft combined price and availability into one scalar via
`score = w_price*price_norm + w_availability*(1-avail)`, with price
min-max-normalized against the OTHER candidates in the same tier. Both
`codex` and `muse` (independently) found this structurally broken: min-max
normalization makes a candidate pair's relative ranking depend on which
OTHER candidates happen to be in the tier at that moment. Verified with a
concrete counterexample (reproduced independently, not just taken from the
reviewer's numbers): targets A(price=1, avail=0.9), B(price=2, avail=1.0),
with equal weights — with a third, irrelevant candidate C(price=101,
avail=1.0) present in the tier, B outranks A; with C removed, A outranks B.
**Neither A's nor B's own price or availability changed — only C's mere
presence flipped the preference between the other two.** This is a classic
"independence of irrelevant alternatives" violation and disqualifies
relative (min-max) normalization as the combining mechanism. The
threshold-gate design sidesteps this entirely: it never produces a scalar
that mixes an absolute quantity (price) with a relative one (normalized
price) and never needs normalization, so no such combination exists to
reverse. It also needs no weight-tuning, which `muse` independently flagged
as "not how a telecom LCR operator actually thinks" (bar first, then
cheapest) versus "cost+quality routing" (a different, weight-tuned thing).

## Availability signal: EWMA success-rate

New, deliberately lightweight package (or a small addition near
`internal/unavail`, same keyspace family) tracking a per-`(provider,
upstream_model)` exponential moving average of success/failure, in Redis.

- **Update rule**: `avail = alpha*outcome + (1-alpha)*avail_prev`, `outcome`
  is `1.0` on success / `0.0` on a tier-attributable failure. `alpha = 0.1`
  fixed (not a config knob — deliberately keeping the parameter surface
  small, per [[feedback_dont_overengineer_distributed_health_tracking]]).
  At this alpha, ~7 consecutive failures roughly halves the value (0.9^7 ≈
  0.48); this is a smoothing signal, not a hard threshold on its own.
- **Cold start**: no stored record → `avail = 1.0`. Optimistic by design —
  a target is never penalized until it has actually failed. This matches
  OpenSIPS's own quality-routing module convention (an insufficiently-
  sampled destination is assumed healthy) and is bounded in practice by
  `internal/unavail`, which already skips a target after its FIRST failure
  (200ms doubling backoff) — a bad new target can win at most one wasted
  attempt per unavail window before LCR's own signal would even matter.
- **Recovery (TTL)**: the Redis key carries a TTL (candidate: 15 minutes —
  shorter than `internal/unavail`'s 1-hour `recordTTL`, since this signal's
  job is to let a demoted target periodically re-earn trust, not remember
  long-term). Without a TTL, a target that fails once while its healthy
  peers keep winning would NEVER receive another attempt to prove it
  recovered (round-robin-on-exact-tie cannot rescue it once its score is no
  longer tied) — both reviewers independently flagged this as a must-fix
  starvation bug in the original design. When the key expires, the next
  read sees "no history" and the target is cold-started back to 1.0,
  competitive again, and earns a real new observation.
- **What counts as an outcome**: reuse `countsAgainstTier`'s existing
  classification exactly (`internal/httpapi/tier_breaker.go`) — the same
  definition `internal/breaker` and `internal/unavail` already use, so all
  three mechanisms agree on "did this target fail." Concretely, in
  `executePlan` (`internal/httpapi/failover.go`): a success updates EWMA
  wherever the existing `recordSuccess` closure already fires (it has two
  call sites — the early streaming-commit path and the `callErr == nil`
  fallback path, see `failover.go:392-419` — hook EWMA update inside that
  one closure rather than adding a second call site); a failure updates
  EWMA inside the existing `case countsAgainstTier(pol, callErr):` branch,
  next to the existing `s.markUnavailable(...)` call. Explicitly excluded
  (inherited for free by hooking into these exact existing branches):
  `errServedWithoutCall` (cache hit, no real call), `ctx.Err() != nil`
  (client went away), and the `default:` request-caused-error branch.
- **Redis access pattern**: follow `internal/breaker`'s pattern (250ms
  per-op timeout, 5s remote-backoff on repeated failure — see
  `breaker.go:180,204`), NOT `internal/unavail`'s (which calls Redis with
  the bare request context and no timeout/backoff of its own). Batch reads:
  one pipelined read for every candidate in the tier being ordered, not a
  sequential GET per candidate — and only when the alias's `lcr_mode`
  actually needs availability (`availability` or `both`; skip the read
  entirely for `lcr_mode = price`). Cache the read snapshot for the
  duration of one `executePlan` call — `Ordered` can be invoked more than
  once across retry passes; don't re-read per pass. Writes are
  fire-and-forget (no optimistic-transaction machinery needed — losing an
  occasional concurrent update under race just slightly slows adaptation,
  which is an acceptable, explicitly accepted tradeoff for a statistical
  smoothing signal, unlike breaker's own correctness-critical counters).
  The success-path write (inside `recordSuccess`, which can fire from the
  streaming first-chunk commit callback) must not be synchronous on that
  callback's return path — it would add latency to time-to-first-byte for
  every streamed response on an `lcr`-strategy alias.
- **Fail-open**: if Redis is unreachable, availability reads return `1.0`
  for every candidate (same as cold start) — `both` and `availability`
  modes degrade to price-only ordering, nothing blocks or errors.

## Price signal

Reuse the existing `pricing.Table` (`internal/pricing/pricing.go`), already
used for billing. Known, deliberate simplifications (documented here, not
silently assumed):

- **No per-request token-split knowledge at routing time.** Proxy price as
  `InputPer1M + 1.0*OutputPer1M` (a fixed, documented, unweighted blend —
  not a tunable ratio; `k=1` is the only honest choice without pretending
  to know an alias's actual traffic mix). This is better than input-only or
  output-only, which can misrank a model whose input/output rates invert
  relative to another's.
- **Unknown-price targets sort LAST, never first.** `pricing.Table`'s
  existing methods (`CostMicroUSD` etc.) return `0` for an unpriced model —
  correct for billing (no record, no charge) but WRONG for ranking (would
  make an unpriced target look free and win all LCR traffic). This needs a
  new, explicit lookup on `pricing.Table` that distinguishes "found, rate X"
  from "not found" (the existing methods don't expose this), and the
  routing code must treat "not found" as effectively infinite price, not
  zero.
- **Context-threshold pricing** (`Price.ContextThreshold`/
  `InputPer1MAbove`/`OutputPer1MAbove` — e.g., Gemini 2.5 Pro's two-tier
  rate) is NOT accounted for: ranking always uses the base (below-threshold)
  rate pair, since prompt size isn't known at routing time for every
  provider path. Documented simplification: an alias mixing long-context
  and short-context traffic across LCR-ranked targets may be mis-ranked
  for the long-context share. Not fixed in v1.
- **Audio targets with per-language model selection**
  (`TargetOptions.RecognitionModels`, a target that picks its own upstream
  model by requested language) are priced/ranked by the target's
  STATICALLY CONFIGURED `UpstreamModel`, not whatever model it actually
  ends up using for a given request's language — billing already correctly
  re-derives the actually-used model per response, but LCR ranking happens
  before that's known. Accepted v1 simplification (operator confirmed).
- **Unit consistency**: only candidates sharing the same `pricing.Unit`
  (`tokens` / `audio_second` / `text_char` — NOT `tts_chars`, corrected
  from an earlier draft) are ever compared, which is automatic here since a
  tier's targets all serve the same alias/request type already.

## Tie-break

Exact-equal scores/prices (common — e.g. `lcr_mode=price` with several
identically-priced targets, or several cold-start `avail=1.0` targets) round-
robin via the SAME `rr` counter `Plan.Ordered` already threads through for
`round_robin`. **Correction from an earlier draft** (caught by `codex`):
rotate only WITHIN each tied sub-group, not the whole tier before a stable
sort — rotating the full tier first and then doing a stable sort by score
does not give equal rotation to a tied pair when an untied third target is
also present in the tier.

## Architecture: keeping `internal/routing` pure

`routing.Plan.Ordered(rr, free)` is a pure function today (`internal/routing/
routing.go`) with two callers: `executePlan` (`internal/httpapi/
failover.go:316`) and `SecondpassChat` (`internal/httpapi/secondpass.go:88`).
Making it perform Redis reads and pricing lookups directly would drag I/O
and two new package dependencies into `internal/routing`, and — more
seriously — `codex` found that pre-admitting every candidate through
`breaker.Admit` just to build an "eligible-only" ranking set would consume
the tier's single probe lease (`breaker.go`'s `minProbeLease = 30s`,
one `Probe:true` admission at a time per tier) before the REAL admission
immediately after, breaking breaker's single-probe invariant.

Design: `httpapi` resolves whatever the chosen `lcr_mode` needs (an
availability snapshot map, when relevant) ONCE per request, then passes a
comparator/rank function into the ordering step — `routing` stays pure and
unaware of Redis or pricing; `httpapi` owns the I/O and policy.

`SecondpassChat` is a genuinely separate, simpler loop — no breaker/unavail
admission, no capacity semaphore, no `executePlan` outcome recording at all,
just `Ordered(...)` followed by a direct provider call. `lcr` ordering
(`price`/`availability`/`both`) still applies there (it only needs the
availability snapshot and pricing table, not the admission machinery), but
it must be documented plainly that `SecondpassChat` has no breaker/unavail
quality-gate backing it — `both` mode's threshold-gate is the only quality
signal in play on that path.

## Required fixes surfaced by review (not optional, block correctness)

- **`internal/httpapi/api_admin.go:587-588`** currently coerces ANY alias
  strategy string other than exactly `"least_busy"` to `"round_robin"` —
  silently destroying an `lcr` save today. Must add `lcr` to this allowlist
  before the feature can work at all, plus the console's strategy dropdown
  (`web/static/app.js`) needs the new option and its mode/threshold fields.
- **Validation**: `lcr_mode` must be one of the three known values (reject
  anything else, don't silently default); `lcr_min_availability` must be
  finite and in `[0,1]`.

## Observability: routing health dashboard

Three independent mechanisms can now influence whether a target gets
traffic — breaker (per alias+tier, binary), `internal/unavail` (per
provider+model, binary), and this EWMA signal (per provider+model,
continuous) — plus price, which was never visible as a routing input
before at all. Without seeing all four together, an operator cannot
explain "why didn't this target get picked" or tune `lcr_min_availability`
sensibly. Operator asked explicitly for a proper dashboard, not a log line
— build real visibility, reusing this project's two already-established
observability patterns (P2, `project_ipsupport_airouter.md`) rather than
inventing a third:

- **Live admin console panel** (same family as the existing "Recent
  requests" panel and the breaker tier-state view): a table, one row per
  `(provider, upstream_model)` that appears in any alias target, showing
  current price (per the proxy above), live EWMA availability %, this
  target's `internal/unavail` state (skipped / remaining backoff), and —
  since breaker is keyed per `(alias, tier)` not per `(provider, model)` —
  the breaker state of every `(alias, tier)` this target currently belongs
  to. New admin API endpoint (e.g. `GET /api/admin/routing/health`) backing
  it; read-only, assembled from the existing registries (pricing table,
  the new EWMA store, `internal/unavail`, `internal/breaker`) — no new
  persistent storage beyond what each mechanism already keeps.
- **Grafana dashboard-as-code** (`deploy/grafana/`, same mechanism P2's
  existing dashboards already use): new gauges `airllm_target_price_usd_per_1m`
  and `airllm_target_availability`, labeled `provider`/`upstream_model`,
  so price and availability trend over time next to the EXISTING
  `airllm_tier_outcomes_total`/breaker metrics already on that dashboard —
  letting the operator correlate "availability dropped" with "tier outcomes
  turned to failure" and "did LCR actually reorder as a result" visually,
  not just at a single point in time.

Both are cheap additions given the infrastructure already exists for each
pattern in this repo; the console panel is for "what's happening right
now", the Grafana panel is for "what happened over time" — distinct
real needs, not redundant.

## Known accepted limitations (documented, not fixed in v1)

- No per-target minimum-sample-count confidence gate (e.g., OpenSIPS's
  "insufficiently sampled = assumed healthy" is already effectively true via
  cold-start=1.0, but there's no explicit "N samples before trusting a LOW
  score either" — a target with exactly one real failure is already treated
  identically to one with a long failure history, once both cross
  `lcr_min_availability`). Acceptable for v1; revisit if it proves too
  twitchy in practice.
- Context-threshold pricing tiers ignored for ranking purposes (base rate
  only — see Price signal above).
- Audio per-language model selection priced/ranked by configured target
  model, not actually-resolved-per-request model (see Price signal above).
- `countsAgainstTier`'s classification is policy-conditioned per target
  (e.g., whether an auth failure counts depends on that target's
  `fallback_on_auth` setting) — the shared `(provider, model)` EWMA
  therefore mixes observations gathered under different per-alias/per-target
  policies. Accepted: the same sharing already exists for `internal/unavail`
  today ("health is a fact about the vendor, not the alias"), and is
  philosophically consistent with it.
