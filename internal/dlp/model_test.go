package dlp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelScan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scan" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"findings":[
			{"label":"PERSON","start":3,"end":9,"score":0.97},
			{"label":"ORG","start":0,"end":2,"score":0.20}
		]}`))
	}))
	defer srv.Close()

	// Trailing slash on the base URL must be handled.
	fs, err := ModelScan(context.Background(), srv.Client(), srv.URL+"/", 0.5, "0123456789")
	if err != nil {
		t.Fatalf("ModelScan: %v", err)
	}
	// ORG is below min score and must be filtered out.
	if len(fs) != 1 {
		t.Fatalf("want 1 finding (PERSON), got %+v", fs)
	}
	if fs[0].Label != "pii:PERSON" || fs[0].Start != 3 || fs[0].End != 9 {
		t.Errorf("unexpected finding %+v", fs[0])
	}
}

func TestModelScanHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := ModelScan(context.Background(), srv.Client(), srv.URL, 0.5, "hi"); err == nil {
		t.Fatal("expected an error on a 5xx sidecar response")
	}
}
