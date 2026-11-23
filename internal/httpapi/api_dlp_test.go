package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestKeepOmittedDLPFields(t *testing.T) {
	cur := dlpConfig{
		Enabled:             true,
		Action:              "redact",
		ScanResponses:       true,
		ModelEnabled:        true,
		ModelURL:            "http://dlp-bert:8000",
		ModelMinScore:       0.7,
		ModelURLs:           []string{"http://a:8000", "http://b:8000"},
		ModelMaxConcurrency: 4,
		ModelScanScope:      "all",
		ModelScanBudgetMS:   3500,
		Patterns:            map[string]bool{"email": true},
		CustomPatterns:      []customPattern{{Label: "ticket", Regex: "TCK-[0-9]+", Enabled: true}},
	}
	apply := func(raw string) dlpConfig {
		t.Helper()
		var body dlpConfig
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		keepOmittedDLPFields([]byte(raw), &body, cur)
		return body
	}

	// A partial PUT changes only what it names.
	got := apply(`{"action":"block"}`)
	want := cur
	want.Action = "block"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("partial PUT:\n got %+v\nwant %+v", got, want)
	}

	// Present keys apply even when empty, zero or false.
	got = apply(`{"enabled":false,"model_enabled":false,"model_url":"","model_urls":[],` +
		`"model_min_score":0,"model_max_concurrency":0,"model_scan_scope":"","model_scan_budget_ms":0,` +
		`"patterns":{},"custom_patterns":[]}`)
	if got.Enabled || got.ModelEnabled || got.ModelURL != "" || len(got.ModelURLs) != 0 ||
		got.ModelMinScore != 0 || got.ModelMaxConcurrency != 0 || got.ModelScanScope != "" || got.ModelScanBudgetMS != 0 ||
		len(got.Patterns) != 0 || len(got.CustomPatterns) != 0 {
		t.Errorf("explicit zero values were not applied: %+v", got)
	}
	if got.Action != "redact" || !got.ScanResponses {
		t.Errorf("omitted fields lost: %+v", got)
	}

	// null is an explicit value too.
	if got = apply(`{"model_urls":null}`); got.ModelURLs != nil || got.ModelURL != cur.ModelURL {
		t.Errorf("model_urls:null: %+v", got)
	}
	// Keys match the way the decoder matched them (case-insensitively).
	if got = apply(`{"Action":"flag"}`); got.Action != "flag" {
		t.Errorf("Action: got %q, want flag", got.Action)
	}
}

// TestHandleAdminPutDLPValidatesMergedConfig: validation sees the stored
// fields a partial PUT left out, so clearing the only sidecar URL of an
// enabled model layer is rejected.
func TestHandleAdminPutDLPValidatesMergedConfig(t *testing.T) {
	s := &Server{}
	s.dlpPtr.Store(&dlpConfig{Enabled: true, Action: "redact", ModelEnabled: true, ModelURL: "http://dlp-bert:8000"})
	req := httptest.NewRequest(http.MethodPut, "/api/admin/dlp", strings.NewReader(`{"model_url":""}`))
	rec := httptest.NewRecorder()
	s.handleAdminPutDLP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clearing model_url with model_enabled stored must be rejected, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminPutDLPRejectsNonObjectBody(t *testing.T) {
	for _, b := range []string{`null`, `[]`, `"x"`, ``} {
		s := &Server{}
		s.dlpPtr.Store(&dlpConfig{Enabled: true, Action: "redact"})
		req := httptest.NewRequest(http.MethodPut, "/api/admin/dlp", strings.NewReader(b))
		rec := httptest.NewRecorder()
		s.handleAdminPutDLP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: got %d, want 400", b, rec.Code)
		}
	}
}

// TestHandleAdminPutDLPKeepsOmittedFields: a policy-only PUT must not wipe
// the stored custom patterns or model settings.
func TestHandleAdminPutDLPKeepsOmittedFields(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var prev []byte
	_ = pool.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'dlp'`).Scan(&prev)
	t.Cleanup(func() {
		if prev != nil {
			_, _ = pool.Exec(context.Background(), `UPDATE settings SET value = $1 WHERE name = 'dlp'`, prev)
		} else {
			_, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE name = 'dlp'`)
		}
	})
	s := &Server{st: &store.Store{PG: pool}, auditHook: func(context.Context, string, string, string, any) {}}
	s.dlpPtr.Store(&dlpConfig{
		Action:            "redact",
		ModelURLs:         []string{"http://dlp-bert:8000"},
		ModelScanBudgetMS: 3500,
		CustomPatterns:    []customPattern{{Label: "ticket", Regex: "TCK-[0-9]+", Enabled: true}},
	})

	body, _ := json.Marshal(map[string]any{"enabled": true, "action": "block"})
	req := httptest.NewRequest(http.MethodPut, "/api/admin/dlp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAdminPutDLP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	got := s.dlpCfg()
	if got.Action != "block" || len(got.CustomPatterns) != 1 || got.CustomPatterns[0].Label != "ticket" {
		t.Fatalf("after policy-only PUT: action=%q custom=%+v, want block + the stored ticket pattern", got.Action, got.CustomPatterns)
	}
	if len(got.ModelURLs) != 1 || got.ModelScanBudgetMS != 3500 {
		t.Fatalf("after policy-only PUT: model_urls=%v budget=%d, want the stored values", got.ModelURLs, got.ModelScanBudgetMS)
	}
}
