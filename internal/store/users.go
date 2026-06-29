package store

import (
	"context"
	"errors"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/jackc/pgx/v5"
)

// PGUsers implements auth.UserStore and the admin user-CRUD queries.
type PGUsers struct{ st *Store }

var _ auth.UserStore = (*PGUsers)(nil)

func NewPGUsers(st *Store) *PGUsers { return &PGUsers{st: st} }

func (p *PGUsers) ByUsername(ctx context.Context, username string) (auth.UserRow, bool, error) {
	var u auth.UserRow
	err := p.st.PG.QueryRow(ctx, `
		SELECT id::text, subject, email, display, roles, password_hash, disabled, auth_source
		FROM users WHERE lower(subject) = lower($1) OR (email <> '' AND lower(email) = lower($1))
		ORDER BY (lower(subject) = lower($1)) DESC LIMIT 1`, username,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.Display, &u.Roles, &u.PasswordHash, &u.Disabled, &u.AuthSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.UserRow{}, false, nil
	}
	return u, err == nil, err
}

func (p *PGUsers) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := p.st.PG.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE NOT disabled AND 'airllm_admin' = ANY(roles)`).Scan(&n)
	return n, err
}

func (p *PGUsers) CreateLocal(ctx context.Context, u auth.UserRow) (string, error) {
	var id string
	err := p.st.PG.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles, password_hash, password_set_at, disabled, auth_source)
		VALUES ($1, $2, $3, $4, $5, now(), $6, 'local')
		RETURNING id::text`,
		u.Subject, u.Email, u.Display, u.Roles, u.PasswordHash, u.Disabled,
	).Scan(&id)
	return id, err
}

func (p *PGUsers) UpsertOIDC(ctx context.Context, pr auth.Principal) (string, error) {
	var id string
	err := p.st.PG.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles, auth_source)
		VALUES ($1, $2, $1, $3, 'oidc')
		ON CONFLICT (subject) DO UPDATE SET email=EXCLUDED.email, roles=EXCLUDED.roles, updated_at=now()
		RETURNING id::text`, pr.Subject, pr.Email, pr.Roles).Scan(&id)
	return id, err
}

func (p *PGUsers) Update(ctx context.Context, id, email, display string, roles []string, disabled bool) error {
	tag, err := p.st.PG.Exec(ctx, `
		UPDATE users SET email=$2, display=$3, roles=$4, disabled=$5, updated_at=now() WHERE id=$1`,
		id, email, display, roles, disabled)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return err
}

func (p *PGUsers) SetPassword(ctx context.Context, id, hash string) error {
	tag, err := p.st.PG.Exec(ctx,
		`UPDATE users SET password_hash=$2, password_set_at=now() WHERE id=$1 AND auth_source='local'`, id, hash)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return err
}

func (p *PGUsers) Delete(ctx context.Context, id string) error {
	tag, err := p.st.PG.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return err
}

// KeyCount returns how many active API keys the user owns (delete guard).
func (p *PGUsers) KeyCount(ctx context.Context, id string) (int, error) {
	var n int
	err := p.st.PG.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id=$1 AND status='active'`, id).Scan(&n)
	return n, err
}

// ByID returns the row for self password-change (verify current password).
func (p *PGUsers) ByID(ctx context.Context, id string) (auth.UserRow, error) {
	var u auth.UserRow
	err := p.st.PG.QueryRow(ctx, `
		SELECT id::text, subject, email, display, roles, password_hash, disabled, auth_source
		FROM users WHERE id=$1`, id,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.Display, &u.Roles, &u.PasswordHash, &u.Disabled, &u.AuthSource)
	return u, err
}

var ErrUserNotFound = errors.New("user not found")
