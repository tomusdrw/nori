package store

import (
	"context"
	"database/sql"
	"strings"

	"deploybot/internal/crypto"
	"github.com/joho/godotenv"
)

// SetEnvFile stores one complete dotenv document. The entire file is encrypted
// because dotenv files commonly contain a mixture of public and secret values.
func (s *Store) SetEnvFile(ctx context.Context, serviceID int64, content string) error {
	stored, err := crypto.Encrypt(s.key, []byte(content))
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO service_env (service_id, content) VALUES (?, ?)
		 ON CONFLICT(service_id) DO UPDATE SET content=excluded.content`,
		serviceID, stored); err != nil {
		return err
	}
	// Once the complete file exists, the legacy per-variable representation is
	// obsolete and must not retain values that were removed from the file.
	if _, err = tx.ExecContext(ctx, `DELETE FROM env_var WHERE service_id=?`, serviceID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetEnvFile returns the complete dotenv document. Per-variable rows created
// by older versions are represented as a dotenv document for compatibility.
func (s *Store) GetEnvFile(ctx context.Context, serviceID int64) (string, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT content FROM service_env WHERE service_id=?`, serviceID).Scan(&raw)
	if err == nil {
		plain, err := crypto.Decrypt(s.key, raw)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	legacy, err := s.ListEnvVars(ctx, serviceID)
	if err != nil || len(legacy) == 0 {
		return "", err
	}
	values := make(map[string]string, len(legacy))
	for _, item := range legacy {
		values[item.Key] = item.Value
	}
	content, err := godotenv.Marshal(values)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(content) + "\n", nil
}

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
