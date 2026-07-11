package httpapi

import (
	"context"
	"fmt"
	"net/http"
)

type providerUsage struct {
	Provider  string  `json:"provider"`
	Requests  int64   `json:"requests"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	P95ms     int64   `json:"p95_ms"`
	Errors    int64   `json:"errors"`
}

type modelUsage struct {
	Alias         string  `json:"alias"`
	Provider      string  `json:"provider"`
	UpstreamModel string  `json:"upstream_model"`
	Requests      int64   `json:"requests"`
	TokensIn      int64   `json:"tokens_in"`
	TokensOut     int64   `json:"tokens_out"`
	CostUSD       float64 `json:"cost_usd"`
	P95ms         int64   `json:"p95_ms"`
	Errors        int64   `json:"errors"`
}

// breakdownProviderQuery and breakdownModelQuery are exported as consts so
// the integration test executes the exact same SQL as the handler (no
// drift). Both take $1 = hours and a %s placeholder for an optional
// caller-supplied WHERE fragment (see usageBreakdown), filled with
// fmt.Sprintf.
const breakdownProviderQuery = `
	SELECT provider_name,
	       count(*),
	       COALESCE(SUM(prompt_tokens), 0),
	       COALESCE(SUM(completion_tokens), 0),
	       COALESCE(SUM(cost_usd), 0),
	       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::bigint,
	       count(*) FILTER (WHERE status >= 400)
	FROM usage_ledger
	WHERE ts > now() - make_interval(hours => $1) AND provider_name <> '' %s
	GROUP BY provider_name
	ORDER BY 5 DESC, 2 DESC`

// The model query keeps rows with an empty provider_name: a request that
// exhausted every target is ledgered without a provider, and hiding it would
// make failures invisible in the breakdown.
const breakdownModelQuery = `
	SELECT alias, provider_name, upstream_model,
	       count(*),
	       COALESCE(SUM(prompt_tokens), 0),
	       COALESCE(SUM(completion_tokens), 0),
	       COALESCE(SUM(cost_usd), 0),
	       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::bigint,
	       count(*) FILTER (WHERE status >= 400)
	FROM usage_ledger
	WHERE ts > now() - make_interval(hours => $1) %s
	GROUP BY alias, provider_name, upstream_model
	ORDER BY 7 DESC, 4 DESC`

// usageBreakdown aggregates the ledger by provider and by model over the last
// `hours`. where is an optional "AND user_id = $2" clause bound after $1,
// mirroring usageSeries.
func (s *Server) usageBreakdown(ctx context.Context, where string, hours int, whereArgs ...any) ([]providerUsage, []modelUsage, error) {
	args := append([]any{hours}, whereArgs...)

	provQ := fmt.Sprintf(breakdownProviderQuery, where)
	rows, err := s.st.PG.Query(ctx, provQ, args...)
	if err != nil {
		return nil, nil, err
	}
	provs := []providerUsage{}
	for rows.Next() {
		var p providerUsage
		if err := rows.Scan(&p.Provider, &p.Requests, &p.TokensIn, &p.TokensOut, &p.CostUSD, &p.P95ms, &p.Errors); err != nil {
			rows.Close()
			return nil, nil, err
		}
		provs = append(provs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	modelQ := fmt.Sprintf(breakdownModelQuery, where)
	rows, err = s.st.PG.Query(ctx, modelQ, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	models := []modelUsage{}
	for rows.Next() {
		var m modelUsage
		if err := rows.Scan(&m.Alias, &m.Provider, &m.UpstreamModel, &m.Requests, &m.TokensIn, &m.TokensOut, &m.CostUSD, &m.P95ms, &m.Errors); err != nil {
			return nil, nil, err
		}
		models = append(models, m)
	}
	return provs, models, rows.Err()
}

// handleUsageBreakdown returns the caller's usage grouped by provider/model.
func (s *Server) handleUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	hours := clampHours(r.URL.Query().Get("hours"))
	provs, models, err := s.usageBreakdown(r.Context(), `AND user_id = $2`, hours, sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage breakdown")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": provs, "models": models})
}

// handleAdminUsageBreakdown returns global usage grouped by provider/model.
func (s *Server) handleAdminUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	hours := clampHours(r.URL.Query().Get("hours"))
	provs, models, err := s.usageBreakdown(r.Context(), "", hours)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage breakdown")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": provs, "models": models})
}
