package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/apikey"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
	"github.com/jackc/pgx/v5"
)

// devUserSubjects are the demo accounts Dev seeds with DevPassword.
var devUserSubjects = []string{"dev-admin", "dev-auditor", "operator"}

// CleanupDev neutralizes demo data left behind by an earlier boot that
// seeded it (older releases seeded on ENV=dev alone): it revokes the
// well-known DevToken API key if it is still active, and disables each demo
// account whose password still verifies as DevPassword. An account whose
// password was changed is left alone. Run on every boot without SEED_DEV;
// a no-op on a clean database, and idempotent.
func CleanupDev(ctx context.Context, st *store.Store) error {
	var keyID string
	err := st.PG.QueryRow(ctx, `
		UPDATE api_keys SET status = 'revoked'
		WHERE hash = $1 AND status = 'active'
		RETURNING id::text`, apikey.Hash(DevToken)).Scan(&keyID)
	switch {
	case err == nil:
		slog.Warn("revoked the well-known dev demo API key left by an earlier dev seed (SEED_DEV is not set)",
			"key_id", keyID)
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("revoke dev demo key: %w", err)
	}

	rows, err := st.PG.Query(ctx, `
		SELECT id::text, subject, password_hash FROM users
		WHERE subject = ANY($1) AND NOT disabled AND password_hash <> ''`, devUserSubjects)
	if err != nil {
		return fmt.Errorf("list dev demo users: %w", err)
	}
	type demoUser struct{ id, subject, hash string }
	var found []demoUser
	for rows.Next() {
		var u demoUser
		if err := rows.Scan(&u.id, &u.subject, &u.hash); err != nil {
			rows.Close()
			return fmt.Errorf("scan dev demo user: %w", err)
		}
		found = append(found, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list dev demo users: %w", err)
	}
	for _, u := range found {
		if !auth.CheckPassword(u.hash, DevPassword) {
			continue
		}
		if _, err := st.PG.Exec(ctx,
			`UPDATE users SET disabled = true, updated_at = now() WHERE id = $1`, u.id); err != nil {
			return fmt.Errorf("disable dev demo user %s: %w", u.subject, err)
		}
		slog.Warn("disabled a dev demo account that still has the well-known dev password (SEED_DEV is not set)",
			"user", u.subject, "user_id", u.id)
	}
	return nil
}
