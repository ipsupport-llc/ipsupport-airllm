// Package ledger persists one usage row per gateway request. The ledger is
// the durable source of truth for reporting and reconciliation.
package ledger

import (
	"context"
	"log/slog"

	"github.com/rromenskyi/ipsupport-airllm/internal/store"
)

// Entry is a single usage record.
type Entry struct {
	KeyID            string // uuid; empty -> NULL
	UserID           string // uuid; empty -> NULL
	Alias            string
	ProviderName     string
	UpstreamModel    string
	IngressProtocol  string
	UpstreamProtocol string
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	Status           int
	LatencyMS        int64
	ErrorMsg         string
}

// Ledger writes usage rows.
type Ledger struct {
	st *store.Store
}

// New returns a Ledger backed by the store.
func New(st *store.Store) *Ledger { return &Ledger{st: st} }

// Record inserts one usage row. It is best-effort: failures are logged, not
// propagated, so metering never breaks the request path.
func (l *Ledger) Record(ctx context.Context, e Entry) {
	_, err := l.st.PG.Exec(ctx, `
		INSERT INTO usage_ledger (
			key_id, user_id, alias, provider_name, upstream_model,
			ingress_protocol, upstream_protocol,
			prompt_tokens, completion_tokens, cost_usd,
			status, latency_ms, error
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		nullUUID(e.KeyID), nullUUID(e.UserID), e.Alias, e.ProviderName, e.UpstreamModel,
		e.IngressProtocol, e.UpstreamProtocol,
		e.PromptTokens, e.CompletionTokens, e.CostUSD,
		e.Status, e.LatencyMS, e.ErrorMsg,
	)
	if err != nil {
		slog.Error("ledger record failed", "err", err, "provider", e.ProviderName, "model", e.UpstreamModel)
	}
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
