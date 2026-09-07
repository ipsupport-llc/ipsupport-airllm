package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// pricingImportTimeout bounds the upstream catalog-pricing fetch.
const pricingImportTimeout = 15 * time.Second

// handleAdminPricingImport pulls a provider's whole catalog pricing (e.g.
// OpenRouter, which publishes per-model USD-per-token prices) and upserts it
// into the pricing table in one transaction.
func (s *Server) handleAdminPricingImport(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	name := r.PathValue("provider")
	entry, ok := s.reg().Get(name)
	if !ok {
		writeControlError(w, http.StatusNotFound, "provider not found")
		return
	}
	lister, ok := entry.Provider.(providers.PricedModelLister)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"imported": 0, "unsupported": true})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), pricingImportTimeout)
	defer cancel()
	prices, err := lister.ListModelPricing(ctx)
	if err != nil {
		writeControlError(w, http.StatusBadGateway, "upstream list model pricing: "+err.Error())
		return
	}

	tx, err := s.st.PG.Begin(r.Context())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to import pricing")
		return
	}
	defer tx.Rollback(r.Context())
	// A catalog publishes flat rates and knows nothing of context tiers, so the
	// upsert leaves the tier columns alone — a hand-entered threshold survives
	// an import. RETURNING reads back what the row ends up holding, so the
	// in-memory table below is set from the stored row rather than from the
	// catalog entry, which would drop that threshold until the next reload.
	type importedPrice struct {
		model string
		price pricing.Price
	}
	stored := make([]importedPrice, 0, len(prices))
	for _, mp := range prices {
		p := pricing.Price{InputPer1M: mp.InputPer1M, OutputPer1M: mp.OutputPer1M}
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO pricing (provider, model, input_per_1m, output_per_1m)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (provider, model) DO UPDATE SET
				input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m, updated_at = now()
			RETURNING unit, context_threshold, input_per_1m_above, output_per_1m_above`,
			name, mp.ID, mp.InputPer1M, mp.OutputPer1M).
			Scan(&p.Unit, &p.ContextThreshold, &p.InputPer1MAbove, &p.OutputPer1MAbove); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to import pricing")
			return
		}
		stored = append(stored, importedPrice{mp.ID, p})
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to import pricing")
		return
	}

	for _, row := range stored {
		s.pricing.Set(name, row.model, row.price)
	}
	s.audit(r.Context(), sess.principal.Subject, "pricing.import", name, map[string]int{"imported": len(prices)})
	writeJSON(w, http.StatusOK, map[string]int{"imported": len(prices)})
}
