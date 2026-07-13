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
	for _, mp := range prices {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO pricing (provider, model, input_per_1m, output_per_1m)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (provider, model) DO UPDATE SET
				input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m, updated_at = now()`,
			name, mp.ID, mp.InputPer1M, mp.OutputPer1M); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to import pricing")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to import pricing")
		return
	}

	for _, mp := range prices {
		s.pricing.Set(name, mp.ID, pricing.Price{InputPer1M: mp.InputPer1M, OutputPer1M: mp.OutputPer1M})
	}
	s.audit(r.Context(), sess.principal.Subject, "pricing.import", name, map[string]int{"imported": len(prices)})
	writeJSON(w, http.StatusOK, map[string]int{"imported": len(prices)})
}
