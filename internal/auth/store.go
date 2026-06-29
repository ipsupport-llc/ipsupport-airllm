package auth

import "context"

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
	UpsertOIDC(ctx context.Context, p Principal) (string, error)
}
