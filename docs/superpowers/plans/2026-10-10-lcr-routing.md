# LCR-style Cost/Availability Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a third within-tier ordering strategy (`lcr`) to AirLLM's alias routing — price-only, availability-only, or threshold-gated-both — plus the EWMA availability signal, admin config, and dashboards it needs.

**Architecture:** A new, Redis-backed `internal/lcr` package tracks a per-`(provider, upstream_model)` EWMA success rate, mirroring `internal/unavail`'s shape but with `internal/breaker`'s timeout/backoff discipline. `routing.Plan.Ordered` grows a `rank` parameter so `internal/routing` stays pure (no Redis, no pricing) while `internal/httpapi` resolves price/availability once per request and supplies the comparator. Admin API and console console get the new alias fields; an existing per-alias health endpoint and a new pair of Grafana gauges make price/availability/breaker/unavail visible together.

**Tech Stack:** Go 1.26, Postgres (pgx/v5), Redis (go-redis/v9), Prometheus client_golang, vanilla JS console, Grafana dashboards-as-code.

**Spec:** `docs/superpowers/specs/2026-10-10-lcr-routing-design.md`

## Global Constraints

- `strategy=lcr` is a THIRD value of `model_aliases.strategy`, additive — `round_robin`/`least_busy` behavior must not change.
- `lcr_mode` ∈ `price` | `availability` | `both`; `both` is a threshold-gate (partition by `lcr_min_availability`, then price-sort each group) — NEVER a weighted-sum score (see spec's "Why not a weighted-sum score" — min-max normalization was proven to reverse rankings based on an irrelevant third candidate).
- `internal/routing` stays a pure package: no Redis client, no `internal/pricing` import. All I/O for ranking is resolved in `internal/httpapi` and passed in as data/closures.
- EWMA `alpha = 0.1` and recovery `TTL = 15 * time.Minute` are fixed Go constants, NOT admin-configurable (operator's explicit choice — keep the parameter surface small). `lcr_min_availability` IS per-alias configurable (default `0.5`).
- Redis access for the new availability signal follows `internal/breaker`'s pattern: each op wrapped in `context.WithTimeout(ctx, 250*time.Millisecond)`. Reads are batched (one pipelined call per tier, never per-candidate). Writes are fire-and-forget (spawned internally, never block the caller). Everything fails open to `avail=1.0` (price-only ordering) if Redis is unreachable.
- An unpriced target sorts LAST under `price`/`both`, never first — `pricing.Table`'s existing `CostMicroUSD` family returns `0` for "unknown", which is correct for billing and wrong for ranking; the new lookup must expose "not found" distinctly.
- Price proxy is `InputPer1M + 1.0*OutputPer1M` — fixed blend, not a tunable ratio.
- Tie-break rotates round-robin WITHIN each tied sub-group only, never the whole tier before a stable sort (this was a caught bug in an earlier draft).
- `go vet ./...`, `gofmt -l .`, and the full test suite (`TEST_DATABASE_URL`/`TEST_REDIS_URL` set) must stay green after every task.

## Review Focus

- A tier with only ONE target under `strategy=lcr` must order trivially (no crash, no panic on an empty candidate set for min/max or partitioning) — this is the actual expected shape of the voice/chat worked examples in the spec, not an edge case to skip.
- Two targets with IDENTICAL price (common for same-vendor multi-model tiers) must round-robin between themselves, not silently always pick array-order-first forever.
- Redis genuinely unreachable (not just empty) during an `lcr_mode=availability` or `both` ordering must not add visible latency or an error to the request — the 250ms timeout exists precisely so this degrades to price-only/stable-order, not a hang.
- A target with NO price (`pricing.Rate` returns `ok=false`) under `lcr_mode=price` must sort after every priced target, even when every priced target is itself more "expensive" in absolute terms than realistic ranges — infinity must actually behave like infinity in the comparator, not just a large finite number that could be exceeded by a confused unit mix.
- Saving an alias with `strategy=lcr` through the admin API must actually persist `lcr` (not silently coerce to `round_robin` the way the pre-existing allowlist bug would) — this is the single most likely silent-breakage regression this feature could introduce, and it must be caught by a test that round-trips a PUT then a GET, not just unit-tested in isolation.

---

### Task 1: Price lookup gap + migration

**Files:**
- Create: `migrations/0019_lcr_routing.sql`
- Modify: `internal/pricing/pricing.go`
- Test: `internal/pricing/pricing_test.go`

**Interfaces:**
- Produces: `func (t *Table) Rate(provider, model string) (rate float64, ok bool)` — the price-ranking proxy. `ok=false` means "no price row found for this provider+model (exact or wildcard)", which later tasks MUST treat as "sorts last", never as `rate=0`.
- Produces: migration `0019_lcr_routing.sql` adding `model_aliases.lcr_mode` (default `'price'`) and `model_aliases.lcr_min_availability` (default `0.5`).

- [ ] **Step 1: Write the failing test for `Rate`**

```go
// internal/pricing/pricing_test.go — add to the existing test file
func TestTableRateBlendsInputAndOutput(t *testing.T) {
	tbl := New()
	tbl.Set("openai", "gpt-nano", Price{InputPer1M: 1.0, OutputPer1M: 2.0})
	rate, ok := tbl.Rate("openai", "gpt-nano")
	if !ok {
		t.Fatal("Rate: ok=false for a priced model")
	}
	if rate != 3.0 { // InputPer1M + 1.0*OutputPer1M
		t.Errorf("Rate = %v, want 3.0 (1.0 + 1.0*2.0)", rate)
	}
}

func TestTableRateFallsBackToWildcardProvider(t *testing.T) {
	tbl := New()
	tbl.Set("", "shared-model", Price{InputPer1M: 5.0, OutputPer1M: 0})
	rate, ok := tbl.Rate("some-provider", "shared-model")
	if !ok || rate != 5.0 {
		t.Errorf("Rate via wildcard = (%v, %v), want (5.0, true)", rate, ok)
	}
}

func TestTableRateUnknownModelReportsNotFound(t *testing.T) {
	tbl := New()
	if _, ok := tbl.Rate("openai", "nonexistent"); ok {
		t.Error("Rate for an unpriced model must report ok=false, not a zero rate — a caller treating ok=false as 0 would rank an unpriced target as free")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pricing/... -run TestTableRate -v`
Expected: FAIL — `Rate` is not defined on `*Table`.

- [ ] **Step 3: Implement `Rate`**

Add to `internal/pricing/pricing.go`, right after the existing `CostMicroUSD` method (same lookup precedence — exact provider+model, then wildcard `""` provider):

```go
// Rate returns the LCR routing price proxy for a model: InputPer1M plus
// OutputPer1M (a fixed, documented 1:1 blend — the actual input/output split
// of a future request is unknown at routing time, so this is an
// approximation, not a prediction). ok is false when no price row exists for
// this provider+model (exact or wildcard) — the caller MUST treat that as
// "unknown, sorts last", never as a zero (free) rate; CostMicroUSD's
// "unknown models cost 0" convention is correct for billing and wrong here.
// Deliberately ignores ContextThreshold/*Above rates: ranking always uses
// the base rate pair, since prompt size isn't known before a request is
// sent. See the spec's "Price signal" section for both simplifications.
func (t *Table) Rate(provider, model string) (rate float64, ok bool) {
	t.mu.RLock()
	p, found := t.prices[key(provider, model)]
	if !found {
		p, found = t.prices[key("", model)]
	}
	t.mu.RUnlock()
	if !found {
		return 0, false
	}
	return p.InputPer1M + p.OutputPer1M, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pricing/... -run TestTableRate -v`
Expected: PASS (all 3 new tests).

- [ ] **Step 5: Write the migration**

```sql
-- migrations/0019_lcr_routing.sql
-- LCR-style within-tier ordering strategy: a new `lcr` value for
-- model_aliases.strategy, with two supporting columns. Both are free to
-- carry any value when strategy != 'lcr' (ignored, same pattern as
-- alias_targets.options' free-form keys) — no DB-level CHECK coupling them
-- to strategy; the admin API validates that relationship on save.
ALTER TABLE model_aliases ADD COLUMN lcr_mode text NOT NULL DEFAULT 'price';
ALTER TABLE model_aliases ADD COLUMN lcr_min_availability double precision NOT NULL DEFAULT 0.5;
```

- [ ] **Step 6: Verify the migration applies**

Run: `docker compose -f deploy/docker-compose.yml up -d --build app` then check the boot log for `"migration applied","version":"0019_lcr_routing.sql"`.

- [ ] **Step 7: Commit**

```bash
git add migrations/0019_lcr_routing.sql internal/pricing/pricing.go internal/pricing/pricing_test.go
git commit -m "feat(pricing): expose a raw price rate for LCR ranking, add lcr_mode/lcr_min_availability columns"
```

---

### Task 2: `internal/lcr` — EWMA availability store

**Files:**
- Create: `internal/lcr/lcr.go`
- Create: `internal/lcr/lcr_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (standalone package, like `internal/unavail`).
- Produces:
  - `type Key struct { Provider, UpstreamModel string }`
  - `func New(rdb *redis.Client) *Store`
  - `func (s *Store) WithClock(now func() time.Time) *Store`
  - `func (s *Store) Record(ctx context.Context, k Key, success bool)` — fire-and-forget; never blocks the caller past the 250ms Redis op budget, and the actual write is spawned so even that budget isn't paid by the caller.
  - `func (s *Store) Snapshot(ctx context.Context, keys []Key) map[Key]float64` — batched read; always returns one entry per input key (missing/errored → `1.0`); never errors.

- [ ] **Step 1: Write the failing tests**

```go
// internal/lcr/lcr_test.go
package lcr

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func testKey() Key { return Key{Provider: "test-provider", UpstreamModel: "test-model"} }

func TestSnapshotUnknownKeyDefaultsToOne(t *testing.T) {
	s := New(nil)
	snap := s.Snapshot(context.Background(), []Key{testKey()})
	if snap[testKey()] != 1.0 {
		t.Errorf("Snapshot of an unrecorded key = %v, want 1.0 (cold start)", snap[testKey()])
	}
}

func TestRecordThenSnapshotEWMA(t *testing.T) {
	dsn := requireRedis(t)
	opt, _ := redis.ParseURL(dsn)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })

	clk := newFakeClock()
	s := New(rdb).WithClock(clk.Now)
	k := Key{Provider: "lcr-test", UpstreamModel: "m"}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k.redisKey()).Err() })

	s.Record(context.Background(), k, false) // one failure from cold start (1.0)
	waitForWrite(t, rdb, k)
	snap := s.Snapshot(context.Background(), []Key{k})
	// alpha=0.1: avail = 0.1*0 + 0.9*1.0 = 0.9
	if got := snap[k]; got < 0.89 || got > 0.91 {
		t.Errorf("after one failure from cold start, avail = %v, want ~0.9", got)
	}
}

func TestRecoveryTTLResetsToColdStart(t *testing.T) {
	dsn := requireRedis(t)
	opt, _ := redis.ParseURL(dsn)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })

	k := Key{Provider: "lcr-ttl-test", UpstreamModel: "m"}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k.redisKey()).Err() })
	s := New(rdb)
	s.Record(context.Background(), k, false)
	waitForWrite(t, rdb, k)
	// Force-expire the key to simulate the TTL having elapsed, rather than
	// sleeping 15 real minutes.
	if err := rdb.Expire(context.Background(), k.redisKey(), time.Millisecond).Err(); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	snap := s.Snapshot(context.Background(), []Key{k})
	if snap[k] != 1.0 {
		t.Errorf("after TTL expiry, avail = %v, want 1.0 (cold start again)", snap[k])
	}
}

func TestSnapshotFailsOpenWithoutRedis(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	s := New(rdb)
	start := time.Now()
	snap := s.Snapshot(context.Background(), []Key{testKey()})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Snapshot with Redis down took %v, want it to fail fast", elapsed)
	}
	if snap[testKey()] != 1.0 {
		t.Errorf("Snapshot with Redis down = %v, want fail-open 1.0", snap[testKey()])
	}
}

func TestRecordDoesNotBlockCaller(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 2 * time.Second, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	s := New(rdb)
	start := time.Now()
	s.Record(context.Background(), testKey(), true)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Record took %v before returning, want it to return immediately (write is fire-and-forget)", elapsed)
	}
}
```

Add this small helper at the bottom of the test file (mirrors `internal/unavail/unavail_test.go`'s `TEST_REDIS_URL` gating):

```go
func requireRedis(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping real-Redis test")
	}
	return dsn
}

// waitForWrite polls briefly for Record's background write to land — Record
// is fire-and-forget by design, so the test can't assume it's visible the
// instant Record returns.
func waitForWrite(t *testing.T, rdb *redis.Client, k Key) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := rdb.Exists(context.Background(), k.redisKey()).Result(); n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Record's background write never landed in Redis within 2s")
}
```

Add `"os"` to the test file's imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go build ./internal/lcr/...`
Expected: FAIL — package `lcr` does not exist yet.

- [ ] **Step 3: Implement `internal/lcr/lcr.go`**

```go
// Package lcr tracks a lightweight, per-(provider, upstream_model)
// exponential moving average of request success, for the LCR-style
// `availability`/`both` within-tier ordering strategy (see
// docs/superpowers/specs/2026-10-10-lcr-routing-design.md). Deliberately
// much simpler than internal/breaker: no windows, no counters, no admission
// state — just a single float per target, read in a batch before ordering
// and written fire-and-forget after each attempt's outcome is known.
//
// A target with no recorded history is treated as fully available (1.0):
// optimistic cold start, matching internal/unavail's existing "never
// penalized until it actually fails" philosophy. The stored record carries
// a TTL shorter than internal/unavail's hour-long one, specifically so a
// target that stops receiving attempts (because it keeps losing the
// ordering to healthier peers) periodically resets to cold-start and
// becomes competitive again — without this, a target demoted once could
// never earn a new observation to recover from, a starvation bug both
// design reviewers (codex and muse) independently flagged.
package lcr

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Key identifies the target an availability figure belongs to.
type Key struct {
	Provider      string
	UpstreamModel string
}

func (k Key) redisKey() string { return "air:lcr:avail:" + k.Provider + ":" + k.UpstreamModel }

// alpha is the EWMA smoothing factor: avail = alpha*outcome + (1-alpha)*prev.
// Fixed, not configurable — see the spec's Global Constraints.
const alpha = 0.1

// recordTTL is how long a mark survives without a new observation before the
// next read sees "no history" and treats the target as cold-started again.
const recordTTL = 15 * time.Minute

// opTimeout bounds every individual Redis call, matching internal/breaker's
// discipline (internal/unavail predates this convention and lacks it).
const opTimeout = 250 * time.Millisecond

type record struct {
	Avail float64 `json:"a"`
}

// Store tracks EWMA availability in Redis, shared across replicas, failing
// open to optimistic (1.0) availability when Redis is unreachable.
type Store struct {
	rdb *redis.Client
	now func() time.Time
}

// New returns a Store. A nil rdb is valid: every Snapshot then returns 1.0
// for every key and Record is a no-op, exactly like an unreachable Redis.
func New(rdb *redis.Client) *Store { return &Store{rdb: rdb, now: time.Now} }

// WithClock overrides the clock for deterministic tests. Returns the
// receiver for chaining.
func (s *Store) WithClock(now func() time.Time) *Store { s.now = now; return s }

// Snapshot reads the current availability of every key in one pipelined
// round trip. A key with no stored record, an unparsable one, or any Redis
// error is reported as 1.0 (fail open / cold start) — Snapshot itself never
// returns an error.
func (s *Store) Snapshot(ctx context.Context, keys []Key) map[Key]float64 {
	out := make(map[Key]float64, len(keys))
	for _, k := range keys {
		out[k] = 1.0
	}
	if s == nil || s.rdb == nil || len(keys) == 0 {
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	pipe := s.rdb.Pipeline()
	cmds := make(map[Key]*redis.StringCmd, len(keys))
	for _, k := range keys {
		cmds[k] = pipe.Get(cctx, k.redisKey())
	}
	if _, err := pipe.Exec(cctx); err != nil && err != redis.Nil {
		slog.Warn("lcr: snapshot pipeline failed, falling back to optimistic availability", "err", err)
		return out
	}
	for k, cmd := range cmds {
		raw, err := cmd.Bytes()
		if err != nil {
			continue // redis.Nil (no record) or a transport error: stays 1.0
		}
		var rec record
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		out[k] = rec.Avail
	}
	return out
}

// Record updates k's EWMA with one outcome and refreshes its TTL. The write
// happens in a detached goroutine with its own bounded context: Record
// itself returns immediately, so calling it from a streaming response's
// first-chunk commit callback never delays that chunk's delivery. A race
// between two concurrent Records on the same key can lose one update — an
// explicitly accepted tradeoff for a statistical smoothing signal, unlike
// internal/breaker's correctness-critical counters.
func (s *Store) Record(ctx context.Context, k Key, success bool) {
	if s == nil || s.rdb == nil {
		return
	}
	go s.record(context.WithoutCancel(ctx), k, success)
}

func (s *Store) record(ctx context.Context, k Key, success bool) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	prev := 1.0
	if raw, err := s.rdb.Get(cctx, k.redisKey()).Bytes(); err == nil {
		var rec record
		if json.Unmarshal(raw, &rec) == nil {
			prev = rec.Avail
		}
	}
	outcome := 0.0
	if success {
		outcome = 1.0
	}
	next := alpha*outcome + (1-alpha)*prev
	raw, err := json.Marshal(record{Avail: next})
	if err != nil {
		return
	}
	if err := s.rdb.Set(cctx, k.redisKey(), raw, recordTTL).Err(); err != nil {
		slog.Warn("lcr: failed to record outcome", "provider", k.Provider, "upstream_model", k.UpstreamModel, "err", err)
	}
}
```

The file's imports are exactly `"context"`, `"encoding/json"`, `"log/slog"`,
`"time"`, and `"github.com/redis/go-redis/v9"` — no `"sync"` needed anywhere
in this package.

- [ ] **Step 4: Run tests to verify they pass**

Run: `TEST_REDIS_URL=redis://127.0.0.1:56379/0 go test ./internal/lcr/... -race -v`
Expected: PASS (adjust the Redis URL to this repo's dev compose Redis port, same as other packages' tests).

- [ ] **Step 5: Commit**

```bash
git add internal/lcr/
git commit -m "feat(lcr): add per-(provider,model) EWMA availability store"
```

---

### Task 3: `routing.Plan` gains LCR ordering

**Files:**
- Modify: `internal/routing/routing.go`
- Create: `internal/routing/lcr_ordering_test.go`

**Interfaces:**
- Consumes: nothing new from Tasks 1–2 (this task is pure, in-memory; `httpapi` wires in `pricing.Rate`/`lcr.Snapshot` results in Task 4).
- Produces:
  - `Plan.LCRMode string` and `Plan.LCRMinAvailability float64` fields, populated by `resolveAlias` from the two new columns.
  - New `Ordered` signature: `func (p *Plan) Ordered(rr uint64, free func(provider string) int, rank func(Target) LCRRank) []Target` — `rank` may be `nil` (both existing callers pass `nil` when `p.Strategy != "lcr"`; Task 4 wires a real one for `lcr`-strategy plans).
  - `type LCRRank struct { Price float64; Availability float64 }` — `Price` is `math.Inf(1)` for an unpriced target (never a finite "large" number, so it compares correctly against any real price).

- [ ] **Step 1: Write the failing tests**

```go
// internal/routing/lcr_ordering_test.go
package routing

import "testing"

func lcrTarget(provider string) Target { return Target{Provider: provider, Tier: 0} }

// TestOrderTierLCRSingleTargetIsNoOp covers the actual shape of the spec's
// worked examples (a tier holding exactly one pinned local target) — it must
// return trivially, never call rank, never panic on an empty min/max or
// partition.
func TestOrderTierLCRSingleTargetIsNoOp(t *testing.T) {
	tier := []Target{lcrTarget("solo")}
	out := orderTierLCR(tier, "both", 0.5, 0, func(Target) LCRRank {
		t.Fatal("rank must not be called for a single-target tier")
		return LCRRank{}
	})
	if len(out) != 1 || out[0].Provider != "solo" {
		t.Fatalf("orderTierLCR on a single-target tier = %v, want the same single target unchanged", out)
	}
}

func TestOrderTierLCRPriceMode(t *testing.T) {
	tier := []Target{lcrTarget("expensive"), lcrTarget("cheap"), lcrTarget("mid")}
	rank := func(tgt Target) LCRRank {
		switch tgt.Provider {
		case "cheap":
			return LCRRank{Price: 1.0, Availability: 1.0}
		case "mid":
			return LCRRank{Price: 5.0, Availability: 1.0}
		default:
			return LCRRank{Price: 100.0, Availability: 1.0}
		}
	}
	out := orderTierLCR(tier, "price", 0, 0, rank)
	got := []string{out[0].Provider, out[1].Provider, out[2].Provider}
	want := []string{"cheap", "mid", "expensive"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOrderTierLCRPriceModeUnpricedSortsLast(t *testing.T) {
	tier := []Target{lcrTarget("priced"), lcrTarget("unpriced")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "unpriced" {
			return LCRRank{Price: math.Inf(1), Availability: 1.0}
		}
		return LCRRank{Price: 2.0, Availability: 1.0}
	}
	out := orderTierLCR(tier, "price", 0, 0, rank)
	if out[0].Provider != "priced" || out[1].Provider != "unpriced" {
		t.Fatalf("order = %v, want [priced, unpriced]", []string{out[0].Provider, out[1].Provider})
	}
}

func TestOrderTierLCRAvailabilityMode(t *testing.T) {
	tier := []Target{lcrTarget("flaky"), lcrTarget("solid")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "flaky" {
			return LCRRank{Price: 1.0, Availability: 0.3}
		}
		return LCRRank{Price: 1.0, Availability: 0.95}
	}
	out := orderTierLCR(tier, "availability", 0, 0, rank)
	if out[0].Provider != "solid" {
		t.Fatalf("order = %v, want solid first (higher availability)", []string{out[0].Provider, out[1].Provider})
	}
}

func TestOrderTierLCRBothModeThresholdGate(t *testing.T) {
	tier := []Target{lcrTarget("cheap-but-flaky"), lcrTarget("pricier-but-healthy")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "cheap-but-flaky" {
			return LCRRank{Price: 1.0, Availability: 0.2} // below the 0.5 threshold
		}
		return LCRRank{Price: 10.0, Availability: 0.9}
	}
	out := orderTierLCR(tier, "both", 0.5, 0, rank)
	if out[0].Provider != "pricier-but-healthy" {
		t.Fatalf("order = %v, want the above-threshold target first even though it's pricier", []string{out[0].Provider, out[1].Provider})
	}
}

func TestOrderTierLCRBothModeSortsByPriceWithinEachGroup(t *testing.T) {
	tier := []Target{lcrTarget("a"), lcrTarget("b"), lcrTarget("c")}
	rank := func(tgt Target) LCRRank {
		switch tgt.Provider {
		case "a":
			return LCRRank{Price: 5.0, Availability: 0.9} // above threshold, pricier
		case "b":
			return LCRRank{Price: 1.0, Availability: 0.9} // above threshold, cheaper
		default:
			return LCRRank{Price: 0.5, Availability: 0.1} // below threshold, cheapest overall
		}
	}
	out := orderTierLCR(tier, "both", 0.5, 0, rank)
	got := []string{out[0].Provider, out[1].Provider, out[2].Provider}
	want := []string{"b", "a", "c"} // above-threshold group by price (b, a), then below-threshold (c)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOrderTierLCRTieBreakRotatesOnlyTiedGroup(t *testing.T) {
	// A and B tie on price; C is strictly cheaper and must always be first.
	// Across 3 ticks of rr, A and B must each lead exactly once between
	// themselves — rotating the WHOLE tier first (a caught bug in an
	// earlier draft) would instead let an untied member shift their
	// relative rotation unevenly.
	tier := []Target{lcrTarget("a"), lcrTarget("b"), lcrTarget("c")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "c" {
			return LCRRank{Price: 0.5, Availability: 1.0}
		}
		return LCRRank{Price: 1.0, Availability: 1.0} // a and b tie
	}
	firsts := map[string]int{}
	for rr := uint64(0); rr < 4; rr++ {
		out := orderTierLCR(tier, "price", 0, rr, rank)
		if out[0].Provider != "c" {
			t.Fatalf("rr=%d: c (strictly cheapest) must always be first, got %v", rr, out[0].Provider)
		}
		firsts[out[1].Provider]++
	}
	if firsts["a"] == 0 || firsts["b"] == 0 {
		t.Errorf("tied group [a,b] never rotated across 4 ticks: %v", firsts)
	}
}

func TestPlanOrderedPassesThroughToLCRWhenStrategyIsLCR(t *testing.T) {
	p := &Plan{Strategy: "lcr", LCRMode: "price", Tiers: [][]Target{{lcrTarget("expensive"), lcrTarget("cheap")}}}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "cheap" {
			return LCRRank{Price: 1.0, Availability: 1.0}
		}
		return LCRRank{Price: 9.0, Availability: 1.0}
	}
	out := p.Ordered(0, nil, rank)
	if out[0].Provider != "cheap" {
		t.Fatalf("Plan.Ordered with strategy=lcr = %v, want cheap first", []string{out[0].Provider, out[1].Provider})
	}
}

func TestPlanOrderedIgnoresRankForNonLCRStrategy(t *testing.T) {
	// A nil free func is fine here because round_robin never calls it.
	p := &Plan{Strategy: "round_robin", Tiers: [][]Target{{lcrTarget("a"), lcrTarget("b")}}}
	out := p.Ordered(0, nil, func(Target) LCRRank { t.Fatal("rank must not be called for a non-lcr strategy"); return LCRRank{} })
	if len(out) != 2 {
		t.Fatalf("got %d targets, want 2", len(out))
	}
}
```

Add `"math"` to the test file's imports (used by
`TestOrderTierLCRPriceModeUnpricedSortsLast`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go build ./internal/routing/...`
Expected: FAIL — `LCRRank`, `orderTierLCR`, and the new `Ordered` signature don't exist yet.

- [ ] **Step 3: Implement**

In `internal/routing/routing.go`, add `"math"` and `"sort"` is already imported. Add near `Target`:

```go
// LCRRank is one target's price/availability figures for the `lcr`
// strategy, supplied by the caller (internal/httpapi) since internal/routing
// has no Redis or pricing access of its own. Price is math.Inf(1) for a
// target with no known price — it must sort after every priced target
// regardless of how large their own prices are, which a finite sentinel
// could not guarantee.
type LCRRank struct {
	Price        float64
	Availability float64
}
```

Add to `Plan`:

```go
	// LCRMode and LCRMinAvailability configure the `lcr` strategy; read only
	// when Strategy == "lcr" (see docs/superpowers/specs/2026-10-10-lcr-routing-design.md).
	LCRMode            string
	LCRMinAvailability float64
```

Change `Ordered`'s signature and body:

```go
// Ordered flattens the tiers into the try-order for one request: tier by
// tier, each tier internally ordered by the strategy. rr rotates round-robin;
// free(provider) drives least-busy; rank supplies price/availability for the
// lcr strategy and is ignored (may be nil) otherwise.
func (p *Plan) Ordered(rr uint64, free func(provider string) int, rank func(Target) LCRRank) []Target {
	var out []Target
	for _, tier := range p.Tiers {
		if p.Strategy == "lcr" {
			out = append(out, orderTierLCR(tier, p.LCRMode, p.LCRMinAvailability, rr, rank)...)
			continue
		}
		out = append(out, orderTier(tier, p.Strategy, rr, free)...)
	}
	return out
}
```

Add the new ordering function (keeping `orderTier` untouched for
`round_robin`/`least_busy`):

```go
// orderTierLCR orders one tier under the `lcr` strategy. price mode sorts by
// price ascending; availability mode sorts by availability descending; both
// partitions into "at/above minAvail" then "below it" and price-sorts each
// group — a threshold gate, never a weighted blend (see the spec for why).
// Exact ties round-robin via rr, rotating only within the tied sub-group (by
// walking the stably-sorted result for runs of adjacent equal elements) so
// an untied neighbor can't skew a tied pair's own rotation.
func orderTierLCR(tier []Target, mode string, minAvail float64, rr uint64, rank func(Target) LCRRank) []Target {
	n := len(tier)
	if n <= 1 {
		return tier
	}
	ranks := make([]LCRRank, n)
	for i, t := range tier {
		ranks[i] = rank(t)
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return lessLCR(ranks, idx[a], idx[b], mode, minAvail) })

	// Rotate each maximal run of adjacent, equally-ranked elements by rr,
	// independently of any other run — a strictly-better or strictly-worse
	// neighbor must never affect a tied pair's own rotation.
	for i := 0; i < n; {
		j := i + 1
		for j < n && tiedLCR(ranks, idx[i], idx[j], mode, minAvail) {
			j++
		}
		if j-i > 1 {
			rot := int(rr % uint64(j-i))
			group := append([]int{}, idx[i:j]...)
			for k := range group {
				idx[i+k] = group[(rot+k)%len(group)]
			}
		}
		i = j
	}

	out := make([]Target, n)
	for i, j := range idx {
		out[i] = tier[j]
	}
	return out
}

// lessLCR orders two tier indices under mode: price ascending, availability
// descending, or (for "both") the at/above-threshold group before the
// below-threshold group, price ascending within each.
func lessLCR(ranks []LCRRank, a, b int, mode string, minAvail float64) bool {
	switch mode {
	case "availability":
		return ranks[a].Availability > ranks[b].Availability
	case "both":
		ga, gb := ranks[a].Availability >= minAvail, ranks[b].Availability >= minAvail
		if ga != gb {
			return ga
		}
		return ranks[a].Price < ranks[b].Price
	default: // "price"
		return ranks[a].Price < ranks[b].Price
	}
}

// tiedLCR reports whether tier indices a and b sort equally under mode — the
// exact condition lessLCR's stable sort already placed adjacent equal
// elements next to each other for.
func tiedLCR(ranks []LCRRank, a, b int, mode string, minAvail float64) bool {
	switch mode {
	case "availability":
		return ranks[a].Availability == ranks[b].Availability
	case "both":
		return (ranks[a].Availability >= minAvail) == (ranks[b].Availability >= minAvail) && ranks[a].Price == ranks[b].Price
	default:
		return ranks[a].Price == ranks[b].Price
	}
}
```

Finally, update `resolveAlias` to select and scan the two new columns, and
set them on the returned `Plan`:

```go
	var strategy, lcrMode string
	var lcrMinAvailability float64
	// ... extend the existing SELECT to also fetch lcr_mode, lcr_min_availability
	// and the existing Scan call to also scan into &lcrMode, &lcrMinAvailability
```

(Follow the exact existing column list/Scan pattern at
`internal/routing/routing.go`'s `resolveAlias` — add `lcr_mode,
lcr_min_availability` to the `SELECT` right after `strategy`, and
`&lcrMode, &lcrMinAvailability` to the `Scan` call in the same position,
then set `LCRMode: lcrMode, LCRMinAvailability: lcrMinAvailability` in the
returned `&Plan{...}` literal.)

- [ ] **Step 4: Update the two existing `Ordered` call sites to compile**

`internal/httpapi/failover.go:316` and `internal/httpapi/secondpass.go:88`
now need a third argument. For THIS task only, pass `nil` at both sites to
keep the build green — Task 4 replaces `nil` with a real rank closure at
the `failover.go` site (and optionally at `secondpass.go`, per the spec's
`SecondpassChat` scope note).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/routing/... -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/routing/routing.go internal/routing/lcr_ordering_test.go internal/httpapi/failover.go internal/httpapi/secondpass.go
git commit -m "feat(routing): add the lcr within-tier ordering strategy"
```

---

### Task 4: Wire LCR into `executePlan` and `SecondpassChat`

**Files:**
- Modify: `internal/httpapi/server.go`
- Modify: `internal/httpapi/failover.go`
- Modify: `internal/httpapi/secondpass.go`
- Modify: `internal/metrics/metrics.go`
- Test: `internal/httpapi/lcr_wiring_test.go` (new)

**Interfaces:**
- Consumes: `lcr.New`/`lcr.Store.Snapshot`/`lcr.Store.Record` (Task 2), `pricing.Table.Rate` (Task 1), `routing.LCRRank`/`Plan.Ordered`'s new signature (Task 3).
- Produces: `s.lcr *lcr.Store` field on `*Server`; `s.metrics.SetTargetPrice`/`SetTargetAvailability` gauge setters, called wherever the rank is computed/recorded, for Task 8's dashboard to read.

- [ ] **Step 1: Wire the field**

In `internal/httpapi/server.go`, add the import `"github.com/ipsupport-llc/ipsupport-airllm/internal/lcr"`, add the field next to the existing ones:

```go
	lcr           *lcr.Store       // per-(provider,model) EWMA availability for the lcr strategy; nil Snapshot returns all-1.0
```

and wire it right after the existing `s.unavail = unavail.New(st.RDB)` line:

```go
	s.lcr = lcr.New(st.RDB)
```

- [ ] **Step 2: Add the two Prometheus gauges**

In `internal/metrics/metrics.go`, add fields to `Metrics`:

```go
	targetPrice        *prometheus.GaugeVec
	targetAvailability *prometheus.GaugeVec
```

Add to the collector list inside `New()`, alongside the existing
`tierOutcomes`/`tierLatency` construction:

```go
		targetPrice: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "airllm_target_price_usd_per_1m", Help: "LCR price proxy (InputPer1M + OutputPer1M) for a target, by provider and upstream model.",
		}, []string{"provider", "upstream_model"}),
		targetAvailability: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "airllm_target_availability", Help: "Live EWMA success rate (0..1) for a target, by provider and upstream model.",
		}, []string{"provider", "upstream_model"}),
```

Add both to the `m.reg.MustRegister(...)` call, and add two nil-safe setters
next to the existing `TierAttempt`/`TierSkipped` methods:

```go
// SetTargetPrice records the current LCR price proxy for a target.
func (m *Metrics) SetTargetPrice(provider, model string, usdPer1M float64) {
	if m == nil {
		return
	}
	m.targetPrice.WithLabelValues(provider, model).Set(usdPer1M)
}

// SetTargetAvailability records a target's live EWMA availability.
func (m *Metrics) SetTargetAvailability(provider, model string, avail float64) {
	if m == nil {
		return
	}
	m.targetAvailability.WithLabelValues(provider, model).Set(avail)
}
```

- [ ] **Step 3: Write the failing wiring test**

```go
// internal/httpapi/lcr_wiring_test.go
package httpapi

import (
	"context"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/lcr"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// TestBuildLCRRankUsesPriceAndAvailability exercises the pure rank-building
// helper in isolation (no Redis, no DB) — it takes a pricing lookup and an
// availability snapshot and produces the routing.LCRRank the ordering code
// in Task 3 consumes.
func TestBuildLCRRankUsesPriceAndAvailability(t *testing.T) {
	priceOf := func(provider, model string) (float64, bool) {
		if provider == "known" {
			return 3.0, true
		}
		return 0, false
	}
	avail := map[lcr.Key]float64{{Provider: "known", UpstreamModel: "m"}: 0.75}
	rank := buildLCRRank(priceOf, avail)

	got := rank(routing.Target{Provider: "known", UpstreamModel: "m"})
	if got.Price != 3.0 || got.Availability != 0.75 {
		t.Errorf("rank(known) = %+v, want {Price:3 Availability:0.75}", got)
	}

	gotUnknown := rank(routing.Target{Provider: "unknown", UpstreamModel: "m"})
	if !math.IsInf(gotUnknown.Price, 1) {
		t.Errorf("rank(unknown).Price = %v, want +Inf", gotUnknown.Price)
	}
	if gotUnknown.Availability != 1.0 {
		t.Errorf("rank(unknown, no avail entry).Availability = %v, want 1.0 (cold start)", gotUnknown.Availability)
	}
}
```

Add `"math"` to this test file's imports.

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/httpapi/... -run TestBuildLCRRank -v`
Expected: FAIL — `buildLCRRank` is not defined.

- [ ] **Step 5: Implement the rank builder and wire it into `executePlan`**

Add to `internal/httpapi/failover.go` (new top-level function, near
`countsAgainstTier`'s neighborhood):

```go
// buildLCRRank closes over a price lookup and a pre-fetched availability
// snapshot to produce the pure routing.LCRRank function internal/routing
// needs — keeping Redis and pricing lookups entirely in httpapi, per the
// spec's "keeping internal/routing pure" section.
func buildLCRRank(priceOf func(provider, model string) (float64, bool), avail map[lcr.Key]float64) func(routing.Target) routing.LCRRank {
	return func(t routing.Target) routing.LCRRank {
		price := math.Inf(1)
		if rate, ok := priceOf(t.Provider, t.UpstreamModel); ok {
			price = rate
		}
		a := 1.0
		if v, ok := avail[lcr.Key{Provider: t.Provider, UpstreamModel: t.UpstreamModel}]; ok {
			a = v
		}
		return routing.LCRRank{Price: price, Availability: a}
	}
}
```

Add `"math"` and `"github.com/ipsupport-llc/ipsupport-airllm/internal/lcr"`
to `failover.go`'s imports.

In `executePlan`, right before the existing `for _, t := range
plan.Ordered(s.router.NextRR(plan.Alias), free)` call (line 316), build the
rank once per call (not once per candidate, and not re-built across retry
passes — matches the spec's "cache the read snapshot" requirement):

```go
	var lcrRank func(routing.Target) routing.LCRRank
	if plan.Strategy == "lcr" {
		var keys []lcr.Key
		for _, tier := range plan.Tiers {
			for _, tgt := range tier {
				keys = append(keys, lcr.Key{Provider: tgt.Provider, UpstreamModel: tgt.UpstreamModel})
			}
		}
		avail := map[lcr.Key]float64{}
		if plan.LCRMode != "price" { // skip the read entirely when price alone decides
			avail = s.lcr.Snapshot(ctx, keys)
		}
		lcrRank = buildLCRRank(s.pricing.Rate, avail)
		for _, k := range keys {
			if rate, ok := s.pricing.Rate(k.Provider, k.UpstreamModel); ok {
				s.metrics.SetTargetPrice(k.Provider, k.UpstreamModel, rate)
			}
			if a, ok := avail[k]; ok {
				s.metrics.SetTargetAvailability(k.Provider, k.UpstreamModel, a)
			}
		}
	}
```

(There is no pre-existing `plan.Tiers`-flattening helper in this file —
confirmed by grep before writing this plan — so the direct double loop
above is the right amount of code, not a shortcut around a missing
utility.)

Change the `Ordered` call itself to pass `lcrRank` instead of `nil`:

```go
			for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), free, lcrRank) {
```

`Server` already exposes the shared pricing table as `s.pricing
*pricing.Table` (`internal/httpapi/server.go:72`) — use it directly, no new
field needed.

- [ ] **Step 6: Hook `Record` into the existing success/failure branches**

Inside `recordSuccess`'s closure body (`failover.go:392`), add, right after
whatever it already does:

```go
			if plan.Strategy == "lcr" {
				s.lcr.Record(ctx, lcr.Key{Provider: t.Provider, UpstreamModel: t.UpstreamModel}, true)
			}
```

Inside the existing `case countsAgainstTier(pol, callErr):` branch
(`failover.go:426`), right next to the existing `s.markUnavailable(ctx,
unavailKey, callErr)` call:

```go
				if plan.Strategy == "lcr" {
					s.lcr.Record(ctx, unavailKey.toLCRKey(), false) // unavailKey is already {Provider, UpstreamModel}
				}
```

(`unavailKey` is already a `unavail.Key{Provider, UpstreamModel}` per the
existing code at that line — either add a tiny
`func (k unavail.Key) toLCRKey() lcr.Key` conversion in `internal/unavail`
or just construct `lcr.Key{Provider: unavailKey.Provider, UpstreamModel:
unavailKey.UpstreamModel}` inline; inline is simpler and avoids a
cross-package method for a two-field struct copy — prefer that.)

Guard both additions with `s.lcr != nil` implicitly via `lcr.Store`'s own
nil-receiver safety (Task 2's `Record` already no-ops on a nil `*Store` or
nil `rdb`) — no extra nil check needed in `failover.go` itself.

- [ ] **Step 7: Wire `SecondpassChat` (optional per-request rank, same helper)**

In `internal/httpapi/secondpass.go`, before the `for _, t := range
plan.Ordered(0, s.freeFunc(reg))` call, build a rank the same way as Step 5
if `plan.Strategy == "lcr"` (reuse `buildLCRRank`), else pass `nil`. Document
in a comment at this call site, per the spec, that `SecondpassChat` has no
breaker/unavail admission gate — `lcr`'s own `both`-mode threshold is the
only quality signal available on this path.

- [ ] **Step 8: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && gofmt -l . && TEST_DATABASE_URL=... TEST_REDIS_URL=... go test ./internal/httpapi/... -race -v`
Expected: PASS, no gofmt output, no vet warnings.

- [ ] **Step 9: Commit**

```bash
git add internal/httpapi/server.go internal/httpapi/failover.go internal/httpapi/secondpass.go internal/httpapi/lcr_wiring_test.go internal/metrics/metrics.go
git commit -m "feat(httpapi): wire the lcr strategy into executePlan and SecondpassChat"
```

---

### Task 5: Admin API — strategy allowlist fix, lcr fields, validation

**Files:**
- Modify: `internal/httpapi/api_admin.go`
- Test: `internal/httpapi/api_alias_options_test.go` (extend existing file)

**Interfaces:**
- Consumes: nothing new.
- Produces: `GET /api/admin/aliases` and `PUT /api/admin/aliases/{alias}` both carry `lcr_mode`/`lcr_min_availability`; saving `strategy: "lcr"` persists it (closes the must-fix allowlist bug).

- [ ] **Step 1: Write the failing test**

```go
// Add to internal/httpapi/api_alias_options_test.go (or a new file in the
// same package following its existing test-server setup pattern — read
// that file first for the exact harness helper names to reuse).
func TestPutAliasStrategyLCRPersists(t *testing.T) {
	s := newTestServerForAliasOptions(t) // reuse this file's existing harness helper
	body := map[string]any{
		"protocol": "openai", "strategy": "lcr", "lcr_mode": "both", "lcr_min_availability": 0.6,
		"targets": []map[string]any{{"priority": 0, "provider": "mock", "upstream_model": "m"}},
	}
	putAliasJSON(t, s, "lcr-persist-test", body) // reuse this file's existing PUT helper, or write one matching its style if none exists
	got := getAliasJSON(t, s, "lcr-persist-test")
	if got["strategy"] != "lcr" {
		t.Fatalf(`strategy = %v, want "lcr" (the admin API must not silently coerce it to round_robin)`, got["strategy"])
	}
	if got["lcr_mode"] != "both" {
		t.Errorf(`lcr_mode = %v, want "both"`, got["lcr_mode"])
	}
	if got["lcr_min_availability"] != 0.6 {
		t.Errorf(`lcr_min_availability = %v, want 0.6`, got["lcr_min_availability"])
	}
}

func TestPutAliasLCRModeMustBeKnownValue(t *testing.T) {
	s := newTestServerForAliasOptions(t)
	body := map[string]any{
		"protocol": "openai", "strategy": "lcr", "lcr_mode": "cheapest-vibes",
		"targets": []map[string]any{{"priority": 0, "provider": "mock", "upstream_model": "m"}},
	}
	status, _ := putAliasExpectStatus(t, s, "lcr-bad-mode", body) // write this tiny helper if the file has none returning the raw status
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown lcr_mode", status)
	}
}

func TestPutAliasLCRMinAvailabilityMustBeInRange(t *testing.T) {
	s := newTestServerForAliasOptions(t)
	body := map[string]any{
		"protocol": "openai", "strategy": "lcr", "lcr_mode": "both", "lcr_min_availability": 1.5,
		"targets": []map[string]any{{"priority": 0, "provider": "mock", "upstream_model": "m"}},
	}
	status, _ := putAliasExpectStatus(t, s, "lcr-bad-threshold", body)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for lcr_min_availability=1.5", status)
	}
}
```

Read `internal/httpapi/api_alias_options_test.go` in full before writing
this — reuse its actual harness/helper names verbatim (the names above are
illustrative; match whatever that file already calls its test server
constructor and PUT/GET helpers).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/httpapi/... -run TestPutAliasLCR -v`
Expected: FAIL (compile error or 404/wrong-status, since none of this exists).

- [ ] **Step 3: Implement**

In `internal/httpapi/api_admin.go`, fix the allowlist
(`handleAdminPutAlias`, currently at line 587-588):

```go
	switch body.Strategy {
	case "least_busy", "lcr":
		// keep as-is
	default:
		body.Strategy = "round_robin"
	}
```

Add fields to the PUT body struct:

```go
		LCRMode              string        `json:"lcr_mode"`
		LCRMinAvailability   *float64      `json:"lcr_min_availability"`
```

(`*float64` so an omitted field is distinguishable from an explicit `0` —
default it to `0.5` when nil, matching the migration's column default.)

Add validation right after the existing `SynthesisCacheTTLS` range check:

```go
	if body.Strategy == "lcr" {
		switch body.LCRMode {
		case "price", "availability", "both":
		default:
			writeControlError(w, http.StatusBadRequest, `lcr_mode must be "price", "availability" or "both"`)
			return
		}
	}
	lcrMinAvailability := 0.5
	if body.LCRMinAvailability != nil {
		lcrMinAvailability = *body.LCRMinAvailability
	}
	if lcrMinAvailability < 0 || lcrMinAvailability > 1 {
		writeControlError(w, http.StatusBadRequest, "lcr_min_availability must be between 0 and 1")
		return
	}
```

Update the `INSERT ... ON CONFLICT` statement to include both new columns
(add `lcr_mode, lcr_min_availability` to the column list, `$11, $12` — or
whatever the next placeholder numbers are — to the `VALUES` list, and
`lcr_mode = EXCLUDED.lcr_mode, lcr_min_availability =
EXCLUDED.lcr_min_availability` to the `DO UPDATE SET` clause), and pass
`body.LCRMode, lcrMinAvailability` as the two new `Exec` arguments.

Update `handleAdminAliases` (the GET handler): add `a.lcr_mode,
a.lcr_min_availability` to its `SELECT`, `lcrMode string` and
`lcrMinAvailability float64` to its scan-target locals and the `Scan(...)`
call, and `LCRMode string \`json:"lcr_mode"\`` /
`LCRMinAvailability float64 \`json:"lcr_min_availability"\`` to the
`aliasView` struct, setting them in the `aliasView{...}` construction.

- [ ] **Step 4: Run tests to verify they pass**

Run: `TEST_DATABASE_URL=... go test ./internal/httpapi/... -run "TestPutAliasStrategyLCRPersists|TestPutAliasLCRModeMustBeKnownValue|TestPutAliasLCRMinAvailabilityMustBeInRange|TestFailoverDefaultsRoundTrip" -v`
Expected: PASS, including the PRE-EXISTING `TestFailoverDefaultsRoundTrip`
(verify it doesn't need updating — if it hardcodes a full alias JSON shape,
it may need the two new fields added, same as it needed updating when
`internal/unavail`'s fields were added; check it rather than assume).

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/api_admin.go internal/httpapi/api_alias_options_test.go
git commit -m "fix(admin): allow the lcr strategy through the alias save allowlist, add lcr_mode/lcr_min_availability"
```

---

### Task 6: Extend the per-alias health view with price/availability/unavail

**Files:**
- Modify: `internal/httpapi/tier_breaker.go`

**Interfaces:**
- Consumes: `pricing.Table.Rate` (Task 1), `lcr.Store.Snapshot` (Task 2), `unavail.Store.Check` (pre-existing).
- Produces: `GET /api/admin/aliases/{alias}/health`'s `targets[]` now carries `price_usd_per_1m` (nullable), `availability` (nullable), `unavailable` + `unavailable_remaining_ms`.

- [ ] **Step 1: Write the failing test**

Find this endpoint's existing test (grep `handleAdminAliasHealth\|aliases/.*health` in
`internal/httpapi/*_test.go`) and add a case alongside the existing ones
that configures an `lcr`-strategy alias, records a known price via the
server's pricing table and a known availability via `s.lcr.Record`, hits
the health endpoint, and asserts the new fields appear with the expected
values. Match that file's existing test-server setup exactly rather than
building a new harness.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/httpapi/... -run TestAliasHealth -v` (or whatever
the existing test function is actually named)
Expected: FAIL — the new fields don't exist in the response yet.

- [ ] **Step 3: Implement**

Extend `tierHealthTarget` (`internal/httpapi/tier_breaker.go:177`):

```go
type tierHealthTarget struct {
	Provider               string   `json:"provider"`
	UpstreamModel          string   `json:"upstream_model"`
	DisplayLabel           string   `json:"display_label,omitempty"`
	PriceUSDPer1M          *float64 `json:"price_usd_per_1m,omitempty"`
	Availability           *float64 `json:"availability,omitempty"`
	Unavailable            bool     `json:"unavailable"`
	UnavailableRemainingMS int64    `json:"unavailable_remaining_ms,omitempty"`
}
```

In `handleAdminAliasHealth`, where each `tierHealthTarget` is built (the
existing `h.Targets = append(h.Targets, tierHealthTarget{...})` line), add
the lookups right before that append:

```go
			target := tierHealthTarget{Provider: t.Provider, UpstreamModel: t.UpstreamModel, DisplayLabel: t.DisplayLabel}
			if rate, ok := s.pricing.Rate(t.Provider, t.UpstreamModel); ok {
				target.PriceUSDPer1M = &rate
			}
			avail := s.lcr.Snapshot(r.Context(), []lcr.Key{{Provider: t.Provider, UpstreamModel: t.UpstreamModel}})[lcr.Key{Provider: t.Provider, UpstreamModel: t.UpstreamModel}]
			target.Availability = &avail
			if remaining, skip := s.unavail.Check(r.Context(), unavail.Key{Provider: t.Provider, UpstreamModel: t.UpstreamModel}); skip {
				target.Unavailable = true
				target.UnavailableRemainingMS = remaining.Milliseconds()
			}
			h.Targets = append(h.Targets, target)
```

Add `"github.com/ipsupport-llc/ipsupport-airllm/internal/lcr"` to this
file's imports (`unavail` is presumably already imported given
`s.unavail.Check` is a pattern used elsewhere in this file's neighborhood —
verify and add it too if not).

- [ ] **Step 4: Run test to verify it passes**

Run the same command as Step 2.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/tier_breaker.go
git commit -m "feat(admin): show price, live availability and unavail state per target in the alias health view"
```

---

### Task 7: Console — strategy option, lcr fields, health table columns

**Files:**
- Modify: `web/static/app.js`
- Modify: `web/tierhealth.test.mjs`

**Interfaces:**
- Consumes: Task 5's PUT/GET shape, Task 6's health response shape.
- Produces: the alias editor can set `strategy=lcr` + `lcr_mode` + `lcr_min_availability`; the tier-health modal shows price/availability/unavail per target.

- [ ] **Step 1: Write/extend the failing test**

Read `web/tierhealth.test.mjs` in full first (it already imports and
exercises `tierHealthRows`, per the comment at `app.js:1096`). Add a case
feeding it a tier whose target carries `price_usd_per_1m`,
`availability`, and `unavailable: true`, asserting the rendered row's
HTML contains the expected price/availability text and an "unavailable"
indicator. Match that file's existing assertion style (likely substring
checks against the returned HTML string) exactly.

- [ ] **Step 2: Run test to verify it fails**

Run: `node --test web/tierhealth.test.mjs` (or this repo's actual JS test
command — check `package.json`/Makefile for the real invocation used by
the `js` CI job before assuming `node --test`)
Expected: FAIL — the new columns aren't rendered yet.

- [ ] **Step 3: Implement**

In `app.js`'s alias editor modal (around line 1202-1205), add the `lcr`
option and the two new fields right after the existing strategy `<select>`:

```html
    <label class="field"><span class="lab">Balancing within a priority tier</span>
      <select id="al-strategy">
        <option value="round_robin" ${a.strategy !== "least_busy" && a.strategy !== "lcr" ? "selected" : ""}>round_robin</option>
        <option value="least_busy" ${a.strategy === "least_busy" ? "selected" : ""}>least_busy</option>
        <option value="lcr" ${a.strategy === "lcr" ? "selected" : ""}>lcr (price / availability)</option>
      </select></label>
    <label class="field"><span class="lab">LCR mode (used only when strategy = lcr)</span>
      <select id="al-lcr-mode">
        <option value="price" ${(a.lcr_mode || "price") === "price" ? "selected" : ""}>price</option>
        <option value="availability" ${a.lcr_mode === "availability" ? "selected" : ""}>availability</option>
        <option value="both" ${a.lcr_mode === "both" ? "selected" : ""}>both (availability gate, then price)</option>
      </select></label>
    <label class="field"><span class="lab">LCR minimum availability (0..1, used only in "both" mode)</span>
      <input id="al-lcr-minavail" type="number" min="0" max="1" step="0.01" value="${esc(String(a.lcr_min_availability ?? 0.5))}" style="width:96px" /></label>
```

In the save handler (around line 1340-1344), add the two fields to the PUT
payload:

```js
      { protocol: $("#al-proto", bg).value, strategy: $("#al-strategy", bg).value, targets: tlist,
        lcr_mode: $("#al-lcr-mode", bg).value, lcr_min_availability: Number($("#al-lcr-minavail", bg).value),
        dlp_model_scan: $("#al-bert", bg).checked, expose_backend_headers: $("#al-expose", bg).checked, dlp_audio_scan: $("#al-dlpaudio", bg).checked,
        session_affinity: $("#al-affinity", bg).checked, session_affinity_ttl_s: affinityTTL,
        synthesis_cache: $("#al-ttscache", bg).checked, synthesis_cache_ttl_s: cacheTTL });
```

In `tierHealthRows` (line 1097), extend the targets cell to show the new
per-target figures instead of just `provider/model`:

```js
function tierHealthRows(tiers) {
  const badge = { closed: "active", open: "revoked", half_open: "admin" };
  const fmt = (s) => (s ? new Date(s).toLocaleString() : "—");
  const targetText = (x) => {
    let s = `${esc(x.provider)}/${esc(x.upstream_model)}`;
    if (x.price_usd_per_1m != null) s += ` ($${Number(x.price_usd_per_1m).toFixed(2)}/1M)`;
    if (x.availability != null) s += ` (${Math.round(Number(x.availability) * 100)}% avail)`;
    if (x.unavailable) s += ` [unavailable ${Math.round(Number(x.unavailable_remaining_ms) / 1000)}s]`;
    return s;
  };
  return (tiers || []).map((t) => {
    const state = t.breaker_enabled ? t.state : "off";
    const quarantined = t.state === "open" || t.state === "half_open";
    return `<tr><td class="mono">p${Number(t.tier)}</td>
      <td class="mono">${(t.targets || []).map(targetText).join(", ") || "—"}</td>
      <td><span class="badge ${badge[state] || "neutral"}">${esc(state === "half_open" ? "probing" : state)}</span></td>
      <td>${esc(quarantined ? t.reason || "" : "") || "—"}</td>
      <td>${quarantined ? esc(fmt(t.open_until)) : "—"}</td>
      <td>${t.cooldown_ms ? `${Math.round(Number(t.cooldown_ms) / 1000)}s` : "—"}</td>
      <td style="text-align:right">${quarantined ? `<button class="btn ghost sm" data-release="${Number(t.tier)}">Release</button>` : ""}</td></tr>`;
  });
}
```

- [ ] **Step 4: Run test to verify it passes**

Same command as Step 2.
Expected: PASS.

- [ ] **Step 5: Manually verify in the dev stack**

Run `docker compose -f deploy/docker-compose.yml up -d --build app`, open
the console, create/edit an alias with `strategy=lcr`, save, reopen it to
confirm the fields round-trip, then open its Health panel and confirm the
new per-target text renders without layout breakage (check both desktop
width and a narrow/mobile emulated width — this console has a documented
history of desktop-only-tested regressions, e.g. the v0.1.23 modal
scrolling bug referenced in project memory).

- [ ] **Step 6: Commit**

```bash
git add web/static/app.js web/tierhealth.test.mjs
git commit -m "feat(console): lcr strategy fields in the alias editor, price/availability/unavail in the health view"
```

---

### Task 8: Grafana dashboard panels + docs

**Files:**
- Modify: `deploy/grafana/dashboards/airllm-failover.json`
- Modify: `docs/configuration.md`

**Interfaces:**
- Consumes: `airllm_target_price_usd_per_1m`/`airllm_target_availability` gauges (Task 4).
- Produces: two new panels on the existing AirLLM Failover dashboard; documented `lcr_mode`/`lcr_min_availability` alias config.

- [ ] **Step 1: Add the two panels**

Open `deploy/grafana/dashboards/airllm-failover.json`, find the highest
existing panel `id` and the layout's current lowest `gridPos.y + gridPos.h`
(to place the new panels below everything else without overlapping), and
append two new panel objects to the `panels` array — a `timeseries` type
(not `state-timeline`, since these are continuous values) for each gauge,
following the EXACT structural shape of an existing non-state-timeline
panel in this same file (check whether the file already has a plain
`timeseries` panel to copy from before inventing the field shape from
scratch — Grafana panel JSON is finicky and copying a working one is far
more reliable than composing fresh). Each new panel's `targets[0].expr`:

```
max by (provider, upstream_model) (airllm_target_price_usd_per_1m{provider=~"$provider"})
```

and

```
max by (provider, upstream_model) (airllm_target_availability{provider=~"$provider"})
```

(Check whether this dashboard already has a `$provider` template variable;
if it only has `$alias`, use that instead, or add a `$provider` variable
following the existing `$alias` variable's exact definition shape — do not
invent a different variable style for just these two panels.)

- [ ] **Step 2: Validate the JSON**

Run: `python3 -c "import json; json.load(open('deploy/grafana/dashboards/airllm-failover.json'))"`
Expected: no error (valid JSON) — Grafana dashboards-as-code in this repo
have no other automated validation, so this is the only mechanical check
available; also visually confirm in a running Grafana if one is reachable
from this environment, otherwise rely on the JSON structure match to an
existing working panel.

- [ ] **Step 3: Document the new alias fields**

In `docs/configuration.md`, find the existing alias-config table (the one
documenting `strategy`, `session_affinity`, etc.) and add two rows:

```markdown
| `lcr_mode` | `price` | no | Only read when `strategy=lcr`. `price` (cheapest first), `availability` (highest live EWMA success rate first), or `both` (targets below `lcr_min_availability` sort last; price decides within each group). |
| `lcr_min_availability` | `0.5` | no | Only read when `strategy=lcr` and `lcr_mode=both`. Range `[0, 1]`. |
```

Add a new subsection after the existing "Target unavailability
(Retry-After)" subsection (same doc, same pattern — a short prose
explanation plus the two worked examples from the spec: voice-local-first,
chat-local-last), titled `#### Cost/availability-aware ordering (lcr)`.
Pull the worked-examples prose directly from
`docs/superpowers/specs/2026-10-10-lcr-routing-design.md`'s "Worked
examples" section rather than re-deriving it.

- [ ] **Step 4: Commit**

```bash
git add deploy/grafana/dashboards/airllm-failover.json docs/configuration.md
git commit -m "docs+observability: Grafana panels and configuration docs for the lcr strategy"
```
