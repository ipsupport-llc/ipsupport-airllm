package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/jackc/pgx/v5"
)

type sessCtx int

const sessCtxKey sessCtx = iota

// session is the resolved control-plane caller.
type session struct {
	principal auth.Principal
	userID    string
}

// requireSession authenticates the caller, loads the backing user row, and
// stores the session on the request context. It never creates a user: a
// cookie whose user row is gone (deleted) or disabled gets a 401.
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
		if errors.Is(err, errSessionUserGone) {
			writeControlError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to load user")
			return
		}
		// A disabled account stops authenticating on its very next request,
		// not just once its session cookie's TTL eventually expires — the
		// cookie is a stateless signed blob with no revocation list, so
		// this per-request DB read is the only place that can catch it.
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
// every request so a role change, disable, or delete takes effect immediately.
type ensuredUser struct {
	id       string
	roles    []string
	disabled bool
}

// errSessionUserGone means the session's user row no longer exists, or its
// subject now belongs to a different user than the one the cookie was issued
// for. requireSession answers it with 401.
var errSessionUserGone = errors.New("session user no longer exists")

// ensureUser loads the session's user row. It is read-only on purpose: users
// are provisioned only by the OIDC callback (UpsertOIDC), the local bootstrap
// admin, and the admin API. Recreating the row here from the stateless cookie
// would bring a deleted user back, with the roles baked into their cookie,
// for the rest of the cookie's TTL. The caller uses the returned roles and
// disabled flag, not the cookie's, as the source of truth for this request.
func (s *Server) ensureUser(ctx context.Context, p auth.Principal) (ensuredUser, error) {
	var u ensuredUser
	err := s.st.PG.QueryRow(ctx, `
		SELECT id::text, roles, disabled FROM users WHERE subject = $1`,
		p.Subject,
	).Scan(&u.id, &u.roles, &u.disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ensuredUser{}, errSessionUserGone
	}
	if err != nil {
		return ensuredUser{}, err
	}
	// Cookies issued before the session carried the user id have no uid;
	// they are matched by subject alone until they expire (12h).
	if p.UserID != "" && p.UserID != u.id {
		return ensuredUser{}, errSessionUserGone
	}
	return u, nil
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
