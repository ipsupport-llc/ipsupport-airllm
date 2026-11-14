package seed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// EnsureDLPDefaults seeds the DLP settings row with deployment-provided
// defaults on FIRST boot only. The stored JSON is partial: loadDLP
// unmarshals it over the compiled defaults, so only the fields given here
// are pinned. An existing 'dlp' row — i.e. any operator save — is never
// touched. maxConcurrency <= 0 omits model_max_concurrency entirely (its
// compiled default, 0, already means "unlimited" — nothing to pin).
//
// Without seeding maxConcurrency, a never-configured alias ran with
// model_max_concurrency's zero value (unlimited per-endpoint scan
// concurrency) regardless of the Helm chart's dlpBert.modelMaxConcurrency —
// that value only reached the running config if an operator copied it into
// the console by hand (as the chart's NOTES used to instruct).
//
// A modelURL also seeds model_enabled: true — deploying the sidecar with a
// URL is the clear intent to turn the layer on. Without this, a fresh
// install ran forever with the sidecar deployed, reachable, and configured,
// but model_enabled defaulting false (defaultDLPConfig does not set it):
// the model scan path never executes, and since it never executes it never
// emits a metric either — "the BERT layer is silently never scanning" is
// indistinguishable from "it's healthy and quiet" until an operator reads
// the admin console and notices the toggle.
func EnsureDLPDefaults(ctx context.Context, q store.Querier, modelURL string, maxConcurrency int) error {
	if modelURL == "" && maxConcurrency <= 0 {
		return nil
	}
	seed := map[string]any{}
	if modelURL != "" {
		seed["model_url"] = modelURL
		seed["model_enabled"] = true
	}
	if maxConcurrency > 0 {
		seed["model_max_concurrency"] = maxConcurrency
	}
	val, _ := json.Marshal(seed)
	if _, err := q.Exec(ctx, `
		INSERT INTO settings (name, value) VALUES ('dlp', $1)
		ON CONFLICT (name) DO NOTHING`, val); err != nil {
		return fmt.Errorf("seed dlp defaults: %w", err)
	}
	return nil
}
