package httpapi

import (
	"context"
	"sync"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/budget"
)

// reportCacheTTL is how long a spend aggregate over the ledger is reused.
// The console polls the users list, the live summary and the groups view
// every ~15 s; without this, each open console would scan the ledger on
// every poll.
const reportCacheTTL = 15 * time.Second

// reportCache keeps recent ledger aggregates in memory, keyed by what they
// cover (report and period start). It is purged on every successful
// control-plane write, so a user or group edit shows at once; new usage
// shows within the TTL. The zero value is ready to use.
type reportCache struct {
	mu      sync.Mutex
	gen     uint64 // bumped by purge; a load that straddles one is not stored
	entries map[string]reportCacheEntry
}

type reportCacheEntry struct {
	at  time.Time
	val any
}

// get returns the cached value for key if it is younger than the TTL, else
// calls load and caches what it returns. Errors are not cached.
func (c *reportCache) get(key string, now time.Time, load func() (any, error)) (any, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < reportCacheTTL && !now.Before(e.at) {
		c.mu.Unlock()
		return e.val, nil
	}
	gen := c.gen
	c.mu.Unlock()

	val, err := load()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.gen == gen {
		if c.entries == nil {
			c.entries = map[string]reportCacheEntry{}
		}
		for k, e := range c.entries { // drop expired keys (e.g. past periods)
			if now.Sub(e.at) >= reportCacheTTL {
				delete(c.entries, k)
			}
		}
		c.entries[key] = reportCacheEntry{at: now, val: val}
	}
	c.mu.Unlock()
	return val, nil
}

// purge forgets everything cached.
func (c *reportCache) purge() {
	c.mu.Lock()
	c.gen++
	c.entries = nil
	c.mu.Unlock()
}

// clock is the server's time source.
func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// cachedSpendByUser is every user's spend over the current day, week and
// month (see spendByUser), through the report cache.
func (s *Server) cachedSpendByUser(ctx context.Context) (map[string]map[budget.Period]periodSpend, error) {
	periods := s.currentPeriods()
	key := "spend:" + periods[budget.Day].Start.Format(time.RFC3339) + ":" +
		periods[budget.Week].Start.Format(time.RFC3339) + ":" + periods[budget.Month].Start.Format(time.RFC3339)
	v, err := s.reports.get(key, s.clock(), func() (any, error) {
		return s.spendByUser(ctx, periods)
	})
	if err != nil {
		return nil, err
	}
	return v.(map[string]map[budget.Period]periodSpend), nil
}
