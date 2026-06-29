package auth

import "context"

// LocalAuth authenticates against bcrypt password hashes in the users table.
type LocalAuth struct {
	store UserStore
	*Session
}

// NewLocalAuth builds a local password authenticator.
func NewLocalAuth(store UserStore, sess *Session) *LocalAuth {
	return &LocalAuth{store: store, Session: sess}
}

// Login validates username/password against the store.
func (l *LocalAuth) Login(username, password string) (Principal, bool) {
	u, ok, err := l.store.ByUsername(context.Background(), username)
	if err != nil || !ok {
		checkAgainstDummy(password) // constant-ish time on not-found
		return Principal{}, false
	}
	if u.Disabled || !CheckPassword(u.PasswordHash, password) {
		return Principal{}, false
	}
	return Principal{Subject: u.Subject, Email: u.Email, Roles: u.Roles}, true
}

// Compile-time interface checks: the embedded *Session promotes
// SetSession/ClearSession/Authenticate, so LocalAuth satisfies both.
var _ LoginProvider = (*LocalAuth)(nil)
var _ Authenticator = (*LocalAuth)(nil)

// EnsureBootstrapAdmin creates an admin user when none exists. If envPassword
// is set it is used (and never returned); otherwise a random password is
// generated and returned so the caller can log it exactly once.
func EnsureBootstrapAdmin(ctx context.Context, store UserStore, username, envPassword string) (created bool, generated string, err error) {
	n, err := store.CountAdmins(ctx)
	if err != nil {
		return false, "", err
	}
	if n > 0 {
		return false, "", nil
	}
	pw := envPassword
	if pw == "" {
		pw = randToken(18)
		generated = pw
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return false, "", err
	}
	if _, err := store.CreateLocal(ctx, UserRow{
		Subject: username, Email: username + "@local", Display: username,
		Roles: []string{AdminRole}, PasswordHash: hash,
	}); err != nil {
		return false, "", err
	}
	return true, generated, nil
}
