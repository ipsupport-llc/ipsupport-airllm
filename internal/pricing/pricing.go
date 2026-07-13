// Package pricing converts token usage into cost. Prices are loaded from the
// pricing table into an in-memory snapshot and looked up by provider+model
// with a wildcard-provider fallback.
package pricing

import (
	"context"
	"math"
	"sync"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// Price is the USD cost per 1M input/output tokens for a model.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// Table is a concurrency-safe price snapshot.
type Table struct {
	mu     sync.RWMutex
	prices map[string]Price
}

// New returns an empty table.
func New() *Table { return &Table{prices: make(map[string]Price)} }

// key builds the composite map key for a provider+model pair. provider ""
// is the wildcard row, matching any provider.
func key(provider, model string) string {
	return provider + "\x00" + model
}

// Load builds a Table from the pricing rows in the store.
func Load(ctx context.Context, st *store.Store) (*Table, error) {
	t := New()
	rows, err := st.PG.Query(ctx, `SELECT provider, model, input_per_1m, output_per_1m FROM pricing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, model string
		var p Price
		if err := rows.Scan(&provider, &model, &p.InputPer1M, &p.OutputPer1M); err != nil {
			return nil, err
		}
		t.prices[key(provider, model)] = p
	}
	return t, rows.Err()
}

// Set replaces the price for a provider+model pair (used by admin updates).
// An empty provider sets the wildcard row.
func (t *Table) Set(provider, model string, p Price) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prices[key(provider, model)] = p
}

// CostMicroUSD returns the cost of a call in integer micro-USD. Looks up an
// exact provider+model match first, then falls back to the wildcard
// (provider "") row for the model. Unknown models cost 0.
func (t *Table) CostMicroUSD(provider, model string, promptTokens, completionTokens int) int64 {
	t.mu.RLock()
	p, ok := t.prices[key(provider, model)]
	if !ok {
		p, ok = t.prices[key("", model)]
	}
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	usd := float64(promptTokens)/1e6*p.InputPer1M + float64(completionTokens)/1e6*p.OutputPer1M
	return int64(math.Round(usd * 1e6))
}
