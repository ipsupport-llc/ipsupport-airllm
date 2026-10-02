package auth

import (
	"context"
	"errors"
)

// ErrLocalUserSubjectConflict is returned by UserStore.UpsertOIDC when the
// IdP-asserted subject already belongs to a local (password) user — refusing
// the upsert instead of silently taking over that account's email/roles.
var ErrLocalUserSubjectConflict = errors.New("a local user already owns this subject")

// UserRow is a control-plane user record.
type UserRow struct {
	ID           string
	Subject      string
	Email        string
	Display      string
	Roles        []string
	PasswordHash string
	Disabled     bool
	AuthSource   string // "local" | "oidc"
}

// UserStore is the persistence the auth providers need. Implemented by
// internal/store.PGUsers.
type UserStore interface {
	ByUsername(ctx context.Context, username string) (UserRow, bool, error) // match subject (ci) or email
	CountAdmins(ctx context.Context) (int, error)
	CreateLocal(ctx context.Context, u UserRow) (string, error) // returns id
	// UpsertOIDC returns ErrLocalUserSubjectConflict if p.Subject already
	// belongs to a local (password) user.
	UpsertOIDC(ctx context.Context, p Principal) (string, error)
}
