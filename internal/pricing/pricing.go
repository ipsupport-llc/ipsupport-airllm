// Package pricing converts token usage into cost. Prices are loaded from the
// pricing table into an in-memory snapshot and looked up by provider+model
// with a wildcard-provider fallback.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// Unit values a price row may carry. Unit says what the per-1,000,000 rates
// count: tokens (default), seconds of audio for STT, or characters of input
// text for TTS — same formula for all three, only the quantity differs.
const (
	UnitTokens      = "tokens"
	UnitAudioSecond = "audio_second"
	UnitTextChar    = "text_char"
)

// Price is the USD cost per 1,000,000 units for a model.
//
// Some vendors charge a second, higher pair of rates once the prompt crosses a
// context threshold — Gemini 2.5 Pro is $1.25/$10.00 up to 200 000 tokens and
// $2.50/$15.00 above it. ContextThreshold and the two *Above rates express
// that, keeping one row per model because that is both how the vendor
// publishes it and how the console presents it.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
	Unit        string

	// ContextThreshold is the prompt-token count above which the vendor
	// switches to InputPer1MAbove/OutputPer1MAbove. 0 means the model has no
	// tier and those two rates are never read — it cannot mean anything else,
	// since every prompt is above zero. Only meaningful for UnitTokens rows.
	ContextThreshold int
	InputPer1MAbove  float64
	OutputPer1MAbove float64
}

// rates returns the input and output rates that apply to a prompt of this
// size. Crossing the threshold reprices the WHOLE request, output included, at
// the higher pair — it is not a blended rate applied to the excess. The
// breakpoint itself is still the low rate, matching the vendor's "up to N
// tokens" wording.
func (p Price) rates(promptTokens int) (in, out float64) {
	if p.ContextThreshold > 0 && promptTokens > p.ContextThreshold {
		return p.InputPer1MAbove, p.OutputPer1MAbove
	}
	return p.InputPer1M, p.OutputPer1M
}

// Validate reports whether the row is a coherent price definition, so an
// operator learns about a mistake in the form rather than from a bill. A tier
// missing one of its rates is the mistake worth catching: it would price a
// long prompt at nothing, which is worse than the flat rate it replaced.
func (p Price) Validate() error {
	switch p.Unit {
	case "", UnitTokens, UnitAudioSecond, UnitTextChar:
	default:
		return fmt.Errorf("unknown unit %q", p.Unit)
	}
	if p.InputPer1M < 0 || p.OutputPer1M < 0 || p.InputPer1MAbove < 0 || p.OutputPer1MAbove < 0 {
		return errors.New("rates cannot be negative")
	}
	if p.ContextThreshold < 0 {
		return errors.New("context threshold cannot be negative")
	}
	if p.ContextThreshold == 0 {
		return nil
	}
	if p.Unit != "" && p.Unit != UnitTokens {
		return fmt.Errorf("a context threshold prices a prompt in tokens and is never read for a %s row", p.Unit)
	}
	if p.InputPer1MAbove <= 0 || p.OutputPer1MAbove <= 0 {
		return errors.New("a context threshold needs both above-threshold rates")
	}
	return nil
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
	rows, err := st.PG.Query(ctx, `SELECT provider, model, input_per_1m, output_per_1m, unit,
		context_threshold, input_per_1m_above, output_per_1m_above FROM pricing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, model string
		var p Price
		if err := rows.Scan(&provider, &model, &p.InputPer1M, &p.OutputPer1M, &p.Unit,
			&p.ContextThreshold, &p.InputPer1MAbove, &p.OutputPer1MAbove); err != nil {
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
//
// The prompt count does double duty: it is a quantity to price, and it selects
// which pair of rates prices the call when the row carries a context tier. The
// tier belongs to whichever row the lookup picked, so a provider-specific row
// is not lent the wildcard row's threshold.
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
	inPer1M, outPer1M := p.rates(promptTokens)
	usd := float64(promptTokens)/1e6*inPer1M + float64(completionTokens)/1e6*outPer1M
	return int64(math.Round(usd * 1e6))
}

// AudioCostMicroUSD prices a transcription by audio duration: seconds/1e6
// * the priced row's InputPer1M (interpreted as $ per 1,000,000 seconds).
// Same lookup precedence as CostMicroUSD: exact provider+model, then the
// wildcard provider "", then 0 for an unpriced model.
func (t *Table) AudioCostMicroUSD(provider, model string, seconds float64) int64 {
	t.mu.RLock()
	p, ok := t.prices[key(provider, model)]
	if !ok {
		p, ok = t.prices[key("", model)]
	}
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	return int64(math.Round(seconds / 1e6 * p.InputPer1M * 1e6))
}

// TTSCostMicroUSD prices a synthesis by input character count: chars/1e6 *
// the priced row's InputPer1M (interpreted as $ per 1,000,000 characters).
// Same lookup precedence as CostMicroUSD.
func (t *Table) TTSCostMicroUSD(provider, model string, chars int) int64 {
	t.mu.RLock()
	p, ok := t.prices[key(provider, model)]
	if !ok {
		p, ok = t.prices[key("", model)]
	}
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	return int64(math.Round(float64(chars) / 1e6 * p.InputPer1M * 1e6))
}
