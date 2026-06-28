package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rromenskyi/ipsupport-airllm/internal/auth"
)

type sessCtx int

const sessCtxKey sessCtx = iota

// session is the resolved control-plane caller.
type session struct {
	principal auth.Principal
	userID    string
}

// requireSession authenticates the caller, ensures a backing user row, and
// stores the session on the request context.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			writeControlError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		uid, err := s.ensureUser(r.Context(), p)
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to load user")
			return
		}
		ctx := context.WithValue(r.Context(), sessCtxKey, session{principal: p, userID: uid})
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin is requireSession plus an admin-role check.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := sessionFrom(r.Context())
		if !sess.principal.IsAdmin() {
			writeControlError(w, http.StatusForbidden, "admin role required")
			return
		}
		next(w, r)
	})
}

// requireAuditor is requireSession plus an auditor-or-admin check.
func (s *Server) requireAuditor(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := sessionFrom(r.Context())
		if !sess.principal.IsAuditor() {
			writeControlError(w, http.StatusForbidden, "auditor role required")
			return
		}
		next(w, r)
	})
}

func (s *Server) ensureUser(ctx context.Context, p auth.Principal) (string, error) {
	var id string
	err := s.st.PG.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (subject) DO UPDATE SET email = EXCLUDED.email, roles = EXCLUDED.roles, updated_at = now()
		RETURNING id::text`,
		p.Subject, p.Email, p.Subject, p.Roles,
	).Scan(&id)
	return id, err
}

func sessionFrom(ctx context.Context) (session, bool) {
	sess, ok := ctx.Value(sessCtxKey).(session)
	return sess, ok
}

func writeControlError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decodeJSON reads a JSON request body into v.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
