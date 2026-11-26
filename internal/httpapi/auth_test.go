package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/apikey"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestLookupKeyRejectsDisabledUser proves a disabled user's otherwise-active
// API key stops authenticating immediately — the bug this guards against:
// lookupKey used to only check api_keys.status, never joining users.disabled,
// so a disabled account's keys kept working on the data plane indefinitely.
func TestLookupKeyRejectsDisabledUser(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Server{st: &store.Store{PG: pool}}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	token := "air_test_" + suffix

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, disabled) VALUES ($1, true) RETURNING id::text`,
		"disabled-key-test-"+suffix,
	).Scan(&userID); err != nil {
		t.Fatalf("insert disabled user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (user_id, hash, prefix, last4, status)
		VALUES ($1, $2, 'air_test', '0000', 'active')`,
		userID, apikey.Hash(token),
	); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	if _, err := s.lookupKey(ctx, token); err == nil {
		t.Fatal("lookupKey must reject a key whose owning user is disabled")
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = false WHERE id = $1`, userID); err != nil {
		t.Fatalf("re-enable user: %v", err)
	}
	if _, err := s.lookupKey(ctx, token); err != nil {
		t.Fatalf("lookupKey must succeed once the owning user is re-enabled, got: %v", err)
	}
}

// TestKeyOwnerLabel: the key's owner is labelled for the console by display
// name, else email, else subject, in the same lookup as the key, and the
// label reaches the owner's in-flight requests in the live view while user
// stays the subject.
func TestKeyOwnerLabel(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Server{st: &store.Store{PG: pool}, live: newLiveRegistry("pod-a")}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	for _, c := range []struct{ name, display, email, want string }{
		{"display", "Alice Example", "alice@example.com", "Alice Example"},
		{"email", "", "bob@example.com", "bob@example.com"},
		{"blank-display", "  ", "carol@example.com", "carol@example.com"},
		{"subject", "", "", ""}, // "" = the subject
	} {
		t.Run(c.name, func(t *testing.T) {
			subject := "37092623598" + suffix + "-" + c.name
			want := c.want
			if want == "" {
				want = subject
			}
			token := "air_test_label_" + suffix + "_" + c.name
			var userID string
			if err := pool.QueryRow(ctx, `
				INSERT INTO users (subject, display, email) VALUES ($1, $2, $3) RETURNING id::text`,
				subject, c.display, c.email,
			).Scan(&userID); err != nil {
				t.Fatalf("insert user: %v", err)
			}
			t.Cleanup(func() {
				bg := context.Background()
				_, _ = pool.Exec(bg, `DELETE FROM api_keys WHERE user_id = $1`, userID)
				_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, userID)
			})
			if _, err := pool.Exec(ctx, `
				INSERT INTO api_keys (user_id, hash, prefix, last4, status)
				VALUES ($1, $2, 'air_test', '0000', 'active')`,
				userID, apikey.Hash(token),
			); err != nil {
				t.Fatalf("insert api key: %v", err)
			}

			ak, err := s.lookupKey(ctx, token)
			if err != nil {
				t.Fatalf("lookupKey: %v", err)
			}
			if ak.Owner != subject || ak.OwnerLabel != want {
				t.Errorf("owner %q label %q, want %q %q", ak.Owner, ak.OwnerLabel, subject, want)
			}

			var seen []liveRequest
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			s.requireAPIKey(s.trackLive("chat", func(http.ResponseWriter, *http.Request) {
				seen = getLive(t, s).Requests
			}))(httptest.NewRecorder(), r)
			if len(seen) != 1 || seen[0].User != subject || seen[0].UserLabel != want {
				t.Errorf("live entries %+v, want one with user %q and user_label %q", seen, subject, want)
			}
		})
	}

	// A pod that predates labels publishes entries without the field.
	var old podSnapshot
	if err := json.Unmarshal([]byte(`{"pod":"old","requests":[{"id":"x","user":"u"}]}`), &old); err != nil || old.Requests[0].UserLabel != "" {
		t.Errorf("old snapshot: %+v %v", old, err)
	}
}
