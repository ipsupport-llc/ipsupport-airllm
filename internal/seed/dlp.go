package seed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// EnsureDLPDefaults seeds the DLP settings row with a deployment-provided
// sidecar URL on FIRST boot only. The stored JSON is partial: loadDLP
// unmarshals it over the compiled defaults, so only model_url is pinned.
// An existing 'dlp' row — i.e. any operator save — is never touched.
func EnsureDLPDefaults(ctx context.Context, q store.Querier, modelURL string) error {
	if modelURL == "" {
		return nil
	}
	val, _ := json.Marshal(map[string]string{"model_url": modelURL})
	if _, err := q.Exec(ctx, `
		INSERT INTO settings (name, value) VALUES ('dlp', $1)
		ON CONFLICT (name) DO NOTHING`, val); err != nil {
		return fmt.Errorf("seed dlp defaults: %w", err)
	}
	return nil
}
