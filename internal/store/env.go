package store

import (
	"context"

	"deploybot/internal/crypto"
)

func (s *Store) SetEnvVar(ctx context.Context, ev *EnvVar) error {
	stored := []byte(ev.Value)
	if ev.IsSecret {
		enc, err := crypto.Encrypt(s.key, []byte(ev.Value))
		if err != nil {
			return err
		}
		stored = enc
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO env_var (service_id, key, value, is_secret) VALUES (?,?,?,?)
		 ON CONFLICT(service_id, key) DO UPDATE SET value=excluded.value, is_secret=excluded.is_secret`,
		ev.ServiceID, ev.Key, stored, boolToInt(ev.IsSecret))
	return err
}

func (s *Store) ListEnvVars(ctx context.Context, serviceID int64) ([]*EnvVar, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, key, value, is_secret FROM env_var WHERE service_id=? ORDER BY key`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EnvVar
	for rows.Next() {
		var ev EnvVar
		var raw []byte
		var isSecret int
		if err := rows.Scan(&ev.ID, &ev.ServiceID, &ev.Key, &raw, &isSecret); err != nil {
			return nil, err
		}
		ev.IsSecret = isSecret != 0
		if ev.IsSecret {
			dec, err := crypto.Decrypt(s.key, raw)
			if err != nil {
				return nil, err
			}
			ev.Value = string(dec)
		} else {
			ev.Value = string(raw)
		}
		out = append(out, &ev)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
