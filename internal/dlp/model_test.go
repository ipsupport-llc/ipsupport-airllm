package dlp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// TestModelScanDrainsBodyOnNonSuccessStatus is the DLP/capture Minor fix:
// an unread non-2xx response body prevents Go's http.Transport from
// reusing the underlying connection, forcing a fresh TCP connection per
// failed scan instead of pooling it — real resource churn, since ModelScan
// runs on every message the BERT layer is enabled for. Proven by actually
// counting accepted connections across repeated requests over a keep-alive
// client: without draining, each request gets its own connection; with it,
// they share one.
func TestModelScanDrainsBodyOnNonSuccessStatus(t *testing.T) {
	var conns int32
	// NewUnstartedServer + manual Start, not NewServer: NewServer starts
	// serving immediately, which races against setting Config.ConnState
	// afterward (caught by -race: the server's own goroutine can read
	// ConnState while this one is still writing it).
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 10*1024*1024)))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	hc := srv.Client()
	for i := 0; i < 3; i++ {
		if _, err := ModelScan(context.Background(), hc, srv.URL, 0.5, "text"); err == nil {
			t.Fatal("expected an error for the 500 response")
		}
	}

	if got := atomic.LoadInt32(&conns); got > 1 {
		t.Errorf("accepted %d TCP connections for 3 sequential requests on a keep-alive client, want 1 — the non-2xx body must be drained to allow connection reuse", got)
	}
}
