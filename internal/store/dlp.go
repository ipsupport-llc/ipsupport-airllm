package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GetSetting returns the raw JSON value for a settings key, or (nil, nil) when
// the key is absent.
func (s *Store) GetSetting(ctx context.Context, name string) ([]byte, error) {
	var v []byte
	err := s.PG.QueryRow(ctx, `SELECT value FROM settings WHERE name = $1`, name).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// GetSettings returns the named settings in one query; an absent name is
// absent from the map.
func (s *Store) GetSettings(ctx context.Context, names []string) (map[string][]byte, error) {
	rows, err := s.PG.Query(ctx, `SELECT name, value FROM settings WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var name string
		var v []byte
		if err := rows.Scan(&name, &v); err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, rows.Err()
}

// PutSetting upserts a settings key.
func (s *Store) PutSetting(ctx context.Context, name string, value []byte) error {
	_, err := s.PG.Exec(ctx, `
		INSERT INTO settings (name, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		name, string(value))
	return err
}

// PutSettingIfAbsent inserts name=value only if no row exists yet, and
// returns the value now stored for name — the caller's own value if it won
// the race, or whatever a concurrent caller already persisted if it lost.
// Used to let several replicas converge on one generated value (e.g. a
// per-install key) without any of them overwriting another's.
func (s *Store) PutSettingIfAbsent(ctx context.Context, name string, value []byte) ([]byte, error) {
	var stored []byte
	err := s.PG.QueryRow(ctx, `
		INSERT INTO settings (name, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (name) DO NOTHING
		RETURNING value`, name, string(value)).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.GetSetting(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	return stored, nil
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DLPIncident is one recorded detection.
type DLPIncident struct {
	KeyID           string
	UserID          string
	IngressProtocol string
	Alias           string
	Action          string
	Labels          []string
	MatchCount      int
	Sample          string
}

// RecordDLPIncident inserts an incident (best-effort caller).
func (s *Store) RecordDLPIncident(ctx context.Context, in DLPIncident) error {
	_, err := s.PG.Exec(ctx, `
		INSERT INTO dlp_incidents
			(key_id, user_id, ingress_protocol, alias, action, labels, match_count, sample)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		nullUUID(in.KeyID), nullUUID(in.UserID), in.IngressProtocol, in.Alias,
		in.Action, in.Labels, in.MatchCount, in.Sample)
	return err
}

// Webhook is an outbound alert endpoint. SecretEnc is the AES-GCM-sealed
// signing secret (nil when the webhook has none) — callers decrypt it with
// the same secrets.Sealer used for provider credentials.
type Webhook struct {
	ID        string
	Name      string
	URL       string
	SecretEnc []byte
	Events    []string
	Enabled   bool
}

// WebhooksForEvent returns enabled webhooks subscribed to an event.
func (s *Store) WebhooksForEvent(ctx context.Context, event string) ([]Webhook, error) {
	rows, err := s.PG.Query(ctx,
		`SELECT id::text, url, secret_enc FROM webhooks WHERE enabled = true AND $1 = ANY(events)`, event)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(&w.ID, &w.URL, &w.SecretEnc); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
