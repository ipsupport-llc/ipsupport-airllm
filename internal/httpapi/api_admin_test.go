package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestProviderConfigToStore covers the two decisions a provider save makes
// about structured configuration: which value to persist when the request
// omits one, and whether the result could ever serve a request.
func TestProviderConfigToStore(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		supplied string
		stored   string
		baseURL  string
		want     string
		wantErr  bool
	}{
		{"a kind that needs no config gets the column default", "openai", "", "", "", "{}", false},
		{"an explicit null is the column default too", "openai", "null", "", "", "{}", false},
		{"an unvalidated kind keeps whatever it was given", "openai", `{"anything":1}`, "", "", `{"anything":1}`, false},
		{"vertex with a project", "vertex", `{"project":"acme","location":"us-west1"}`, "", "", `{"project":"acme","location":"us-west1"}`, false},
		{"a supplied config replaces the stored one", "vertex", `{"project":"new"}`, `{"project":"old"}`, "", `{"project":"new"}`, false},
		{"an omitted config keeps the stored one", "vertex", "", `{"project":"acme","location":"global"}`, "", `{"project":"acme","location":"global"}`, false},
		{"vertex with only an explicit address", "vertex", "{}", "", "http://127.0.0.1:8080", "{}", false},
		{"vertex with neither is rejected on save", "vertex", "{}", "", "", "", true},
		{"vertex with nothing anywhere is rejected on save", "vertex", "", "", "", "", true},
		{"vertex with a malformed config is rejected on save", "vertex", `{"project":`, "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := providerConfigToStore(c.kind, json.RawMessage(c.supplied), c.stored, c.baseURL)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("providerConfigToStore: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestCredentialToStore covers what a provider save does to the stored
// credential. The asymmetry is the point: a save that says nothing keeps the
// stored credential, so only an explicit clear can remove one, and a request
// cannot both set and clear.
func TestCredentialToStore(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string
		credJSON   string
		clear      bool
		wantSecret string
		wantChange credentialChange
		wantErr    bool
	}{
		{"a save that says nothing keeps the stored credential", "", "", false, "", credentialKept, false},
		{"an api key replaces it", "sk-1", "", false, "sk-1", credentialSet, false},
		{"a service-account key replaces it", "", `{"type":"service_account"}`, false, `{"type":"service_account"}`, credentialSet, false},
		{"an explicit clear removes it", "", "", true, "", credentialCleared, false},
		{"both credential fields are rejected", "sk-1", "{}", false, "", "", true},
		{"clearing while setting an api key is rejected", "sk-1", "", true, "", "", true},
		{"clearing while setting a service-account key is rejected", "", "{}", true, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secret, change, err := credentialToStore(c.apiKey, c.credJSON, c.clear)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q (%s)", secret, change)
				}
				return
			}
			if err != nil {
				t.Fatalf("credentialToStore: %v", err)
			}
			if secret != c.wantSecret || change != c.wantChange {
				t.Errorf("got (%q, %s), want (%q, %s)", secret, change, c.wantSecret, c.wantChange)
			}
		})
	}
}

// TestPutProviderRejectsNegativeMaxConcurrency is the Admin API Minor fix:
// 0 means unlimited concurrency (internal/providers.Registry.Register — a
// nil semaphore lets every Acquire succeed), so silently clamping a negative
// max_concurrency to 0 turned a fat-fingered or buggy request into the MOST
// permissive possible outcome instead of rejecting it. This never reaches
// the database — the check is the first thing after body decode — so a
// zero-value *Server is enough to exercise it.
func TestPutProviderRejectsNegativeMaxConcurrency(t *testing.T) {
	s := &Server{}
	body := `{"kind":"openai","base_url":"https://api.openai.com/v1","enabled":true,"max_concurrency":-1}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/x", strings.NewReader(body))
	req.SetPathValue("name", "x")
	rec := httptest.NewRecorder()
	s.handleAdminPutProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_concurrency") {
		t.Errorf("error body = %s, want it to name max_concurrency", rec.Body.String())
	}
}

// TestPutProviderSerializesConcurrentSaves proves a concurrent save of the
// same provider does not silently discard another transaction's genuine
// concurrent change (Admin API I1 fix): it holds an UPDATE open in another
// transaction (simulating a slow in-flight save that already changed the
// config but hasn't committed yet), then calls handleAdminPutProvider for
// the SAME provider with config OMITTED — "keep whatever is stored". The
// held transaction's row lock forces the handler to block; once it commits,
// the handler's read MUST see the held transaction's committed value, not
// whatever was there before it even started, or "keep stored" silently
// reverts a real concurrent change.
func TestPutProviderSerializesConcurrentSaves(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sl := testAuditSealer(t)
	name := fmt.Sprintf("race-test-%d", time.Now().UnixNano())

	if _, err := pool.Exec(ctx, `
		INSERT INTO providers (name, kind, base_url, enabled, max_concurrency, config)
		VALUES ($1, 'openai', 'https://api.openai.com/v1', true, 1, '{"a":1}'::jsonb)`,
		name,
	); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM providers WHERE name = $1`, name)
	})

	heldTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin held tx: %v", err)
	}
	if _, err := heldTx.Exec(ctx, `UPDATE providers SET config = '{"a":99}'::jsonb WHERE name = $1`, name); err != nil {
		t.Fatalf("held tx update: %v", err)
	}

	s := &Server{st: &store.Store{PG: pool}, sealer: sl}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// config omitted: "keep whatever is already stored".
		body := `{"kind":"openai","base_url":"https://api.openai.com/v1","enabled":true,"max_concurrency":1}`
		req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/"+name, strings.NewReader(body))
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		s.handleAdminPutProvider(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("concurrent save failed: %d %s", rec.Code, rec.Body.String())
		}
	}()

	select {
	case <-done:
		t.Fatal("concurrent save completed before the holding transaction released its lock — the read isn't serialized against the write")
	case <-time.After(150 * time.Millisecond):
	}

	if err := heldTx.Commit(ctx); err != nil {
		t.Fatalf("release held tx: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent save never completed after the lock was released")
	}

	var finalConfig string
	if err := pool.QueryRow(ctx, `SELECT config::text FROM providers WHERE name = $1`, name).Scan(&finalConfig); err != nil {
		t.Fatalf("read final config: %v", err)
	}
	var got map[string]int
	if err := json.Unmarshal([]byte(finalConfig), &got); err != nil {
		t.Fatalf("unmarshal final config %q: %v", finalConfig, err)
	}
	if got["a"] != 99 {
		t.Fatalf("expected the held transaction's committed config {\"a\":99} to survive a concurrent \"keep stored\" save, got %s — lost update", finalConfig)
	}
}
