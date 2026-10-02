package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
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
		ensureUser := s.ensureUser
		if s.ensureUserFn != nil {
			ensureUser = s.ensureUserFn
		}
		u, err := ensureUser(r.Context(), p)
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to load user")
			return
		}
		// A disabled account stops authenticating on its very next request,
		// not just once its session cookie's TTL eventually expires — the
		// cookie is a stateless signed blob with no revocation list, so
		// this per-request DB read (already happening below for ensureUser
		// regardless) is the only place that can catch it.
		if u.disabled {
			writeControlError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Authorization uses the user's CURRENT roles from the DB, not
		// whatever the cookie baked in at login — otherwise a role change
		// (including a demotion) would silently keep the old privilege
		// level until the cookie's TTL expires or the user logs in again.
		p.Roles = u.roles
		ctx := context.WithValue(r.Context(), sessCtxKey, session{principal: p, userID: u.id})
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

// ensuredUser is the backing DB row for a session's principal, read fresh on
// every request so a role change or disable takes effect immediately.
type ensuredUser struct {
	id       string
	roles    []string
	disabled bool
}

func (s *Server) ensureUser(ctx context.Context, p auth.Principal) (ensuredUser, error) {
	// Resolve the user id by subject. On an existing row, touch only updated_at —
	// never overwrite roles/email from the stateless session cookie, or an admin's
	// role change to an active user would be silently reverted on their next request.
	// (OIDC refreshes roles in the callback's UpsertOIDC before the session is set;
	// local users are admin-managed in the DB.) The caller uses the returned roles
	// and disabled flag — not the cookie's — as the source of truth for this request.
	var u ensuredUser
	err := s.st.PG.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (subject) DO UPDATE SET updated_at = now()
		RETURNING id::text, roles, disabled`,
		p.Subject, p.Email, p.Subject, p.Roles,
	).Scan(&u.id, &u.roles, &u.disabled)
	return u, err
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
