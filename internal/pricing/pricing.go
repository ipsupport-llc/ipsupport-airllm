// Package pricing converts token usage into cost. Prices are loaded from the
// pricing table into an in-memory snapshot and looked up by upstream model.
package pricing

import (
	"context"
	"math"
	"sync"

	"github.com/rromenskyi/ipsupport-airllm/internal/store"
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

// Load builds a Table from the pricing rows in the store.
func Load(ctx context.Context, st *store.Store) (*Table, error) {
	t := New()
	rows, err := st.PG.Query(ctx, `SELECT model, input_per_1m, output_per_1m FROM pricing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var p Price
		if err := rows.Scan(&model, &p.InputPer1M, &p.OutputPer1M); err != nil {
			return nil, err
		}
		t.prices[model] = p
	}
	return t, rows.Err()
}

// Set replaces the price for a model (used by admin updates).
func (t *Table) Set(model string, p Price) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prices[model] = p
}

// CostMicroUSD returns the cost of a call in integer micro-USD. Unknown
// models cost 0.
func (t *Table) CostMicroUSD(model string, promptTokens, completionTokens int) int64 {
	t.mu.RLock()
	p, ok := t.prices[model]
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	usd := float64(promptTokens)/1e6*p.InputPer1M + float64(completionTokens)/1e6*p.OutputPer1M
	return int64(math.Round(usd * 1e6))
}
