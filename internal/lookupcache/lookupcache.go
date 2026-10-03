// Package lookupcache keeps the answers to per-request database lookups (API
// keys, alias plans) in memory for a short TTL, so the hot path does not ask
// the database on every request and a brief database outage does not reject
// every request.
//
// An entry is fresh for TTL; after that the next Get asks the database again.
// When the database cannot answer, the last good answer keeps being served
// until it is MaxStale old. A lookup the database answers with "no such
// thing" (see Miss) evicts the entry at once, so revocations and deletions
// take effect within TTL on every instance. Misses are never cached: an
// unknown key is asked for every time, and rejected while the database is
// down.
package lookupcache

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// refreshTimeout bounds a refresh that has a stale answer to fall back on, so
// a database that hangs instead of refusing does not hold the request.
// loadTimeout bounds one with nothing to fall back on.
const (
	refreshTimeout = 2 * time.Second
	loadTimeout    = 10 * time.Second
)

// backoff is how long, after a failed refresh, stale answers are served
// without asking the database again — during an outage only one request per
// backoff pays for discovering it is still down.
const backoff = 5 * time.Second

// Options configures a Cache.
type Options struct {
	Name     string           // for log lines, e.g. "api keys"
	TTL      time.Duration    // how long an answer is trusted without asking again
	MaxStale time.Duration    // how old an answer may get while the database is unreachable
	Now      func() time.Time // nil = time.Now; tests inject a fake clock
}

// Cache maps a string key to the last good answer of a database lookup. A
// nil *Cache caches nothing: Get calls load every time.
type Cache[V any] struct {
	opts   Options
	flight singleflight.Group // concurrent loads of one key share one query

	mu         sync.Mutex
	entries    map[string]entry[V]
	gen        uint64    // bumped by Purge; a load that straddles a purge is not stored
	quietUntil time.Time // set after a failed refresh; see backoff
}

type entry[V any] struct {
	v  V
	at time.Time
}

// New returns an empty cache.
func New[V any](opts Options) *Cache[V] {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache[V]{opts: opts, entries: map[string]entry[V]{}}
}

type missErr struct{ err error }

func (m missErr) Error() string { return m.err.Error() }
func (m missErr) Unwrap() error { return m.err }

// Miss marks a load error as the database's definitive "no such thing" —
// not found, revoked, disabled — as opposed to the database being unable to
// answer. A Miss evicts the cached entry; any other error falls back to it.
func Miss(err error) error { return missErr{err} }

// Get returns the answer for key, calling load when the cached one is missing
// or older than TTL. load's errors are returned as is.
func (c *Cache[V]) Get(ctx context.Context, key string, load func(context.Context) (V, error)) (V, error) {
	if c == nil {
		return load(ctx)
	}
	now := c.opts.Now()
	c.mu.Lock()
	e, ok := c.entries[key]
	gen := c.gen
	quiet := now.Before(c.quietUntil)
	c.mu.Unlock()

	if ok && now.Sub(e.at) < c.opts.TTL {
		return e.v, nil
	}
	usable := ok && now.Sub(e.at) < c.opts.MaxStale
	if usable && quiet {
		return e.v, nil
	}

	// The shared load must not die with whichever request started it, so it
	// runs detached from the caller and bounded by its own timeout; each
	// caller still stops waiting when its own request ends.
	timeout := loadTimeout
	if usable {
		timeout = refreshTimeout
	}
	ch := c.flight.DoChan(key, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		v, err := load(loadCtx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil && c.gen == gen {
			c.entries[key] = entry[V]{v: v, at: now}
		}
		var miss missErr
		if errors.As(err, &miss) {
			delete(c.entries, key)
		}
		return v, err
	})
	var res singleflight.Result
	select {
	case res = <-ch:
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
	v, _ := res.Val.(V)
	err := res.Err

	var miss missErr
	if err == nil || errors.As(err, &miss) || !usable {
		return v, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		// Purged while the refresh was failing: the old answer was meant to go.
		return v, err
	}
	c.quietUntil = c.opts.Now().Add(backoff)
	slog.Warn("lookup cache: database unavailable, serving the last known answer",
		"cache", c.opts.Name, "age_s", int(now.Sub(e.at).Seconds()), "err", err)
	return e.v, nil
}

// Purge drops every entry, so the next Get of each key asks the database.
// Called after a change made through this instance's admin API, which then
// takes effect here immediately instead of within TTL.
func (c *Cache[V]) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]entry[V]{}
	c.gen++
	c.quietUntil = time.Time{}
}
