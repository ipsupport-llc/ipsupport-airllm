package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestKeepOmittedDLPPatterns(t *testing.T) {
	cur := dlpConfig{
		Patterns:       map[string]bool{"email": true},
		CustomPatterns: []customPattern{{Label: "ticket", Regex: "TCK-[0-9]+", Enabled: true}},
	}
	cases := []struct {
		name       string
		raw        string
		wantPat    int
		wantCustom int
	}{
		{"both omitted keeps both", `{"enabled":true,"action":"redact"}`, 1, 1},
		{"explicit empty clears both", `{"action":"redact","patterns":{},"custom_patterns":[]}`, 0, 0},
		{"only custom sent", `{"action":"redact","custom_patterns":[]}`, 1, 0},
		{"only patterns sent", `{"action":"redact","patterns":{}}`, 0, 1},
	}
	for _, c := range cases {
		var body dlpConfig
		if err := json.Unmarshal([]byte(c.raw), &body); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		keepOmittedDLPPatterns([]byte(c.raw), &body, cur)
		if len(body.Patterns) != c.wantPat || len(body.CustomPatterns) != c.wantCustom {
			t.Errorf("%s: patterns=%d custom=%d, want %d/%d", c.name,
				len(body.Patterns), len(body.CustomPatterns), c.wantPat, c.wantCustom)
		}
	}
}

// TestHandleAdminPutDLPKeepsOmittedCustomPatterns: a policy-only PUT (no
// patterns / custom_patterns keys) must not wipe what is stored.
func TestHandleAdminPutDLPKeepsOmittedCustomPatterns(t *testing.T) {
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
		Action:         "redact",
		CustomPatterns: []customPattern{{Label: "ticket", Regex: "TCK-[0-9]+", Enabled: true}},
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
}
