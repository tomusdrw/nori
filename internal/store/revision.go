package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"deploybot/internal/crypto"
)

// Script revisions follow every write path, including launcher refreshes.
// Environment revisions are recorded in Go so identical plaintext does not
// produce a new version just because encryption generated a new nonce.
const revisionSchema = `
CREATE TABLE IF NOT EXISTS config_revision (
 service_id INTEGER NOT NULL REFERENCES service(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('script', 'env')),
 version INTEGER NOT NULL,
 content BLOB NOT NULL,
 created_at INTEGER NOT NULL,
 PRIMARY KEY(service_id, kind, version)
);
INSERT INTO config_revision(service_id,kind,version,content,created_at)
 SELECT id,'script',1,deploy_script,updated_at FROM service s
 WHERE NOT EXISTS (SELECT 1 FROM config_revision WHERE service_id=s.id AND kind='script');
CREATE TRIGGER IF NOT EXISTS script_revision_insert AFTER INSERT ON service BEGIN
 INSERT INTO config_revision VALUES (NEW.id,'script',1,NEW.deploy_script,NEW.created_at);
END;
CREATE TRIGGER IF NOT EXISTS script_revision_update AFTER UPDATE OF deploy_script ON service
 WHEN OLD.deploy_script != NEW.deploy_script BEGIN
 INSERT INTO config_revision SELECT NEW.id,'script',COALESCE(MAX(version),0)+1,NEW.deploy_script,NEW.updated_at
 FROM config_revision WHERE service_id=NEW.id AND kind='script';
END;
`

type ConfigRevision struct {
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Content   string    `json:"content,omitempty"`
}

// ListConfigRevisions returns metadata only, newest first.
func (s *Store) ListConfigRevisions(ctx context.Context, serviceID int64, kind string) ([]ConfigRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version,created_at FROM config_revision WHERE service_id=? AND kind=? ORDER BY version DESC`, serviceID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConfigRevision{}
	for rows.Next() {
		var item ConfigRevision
		var created int64
		if err := rows.Scan(&item.Version, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetConfigRevision(ctx context.Context, serviceID int64, kind string, version int64) (*ConfigRevision, error) {
	var item ConfigRevision
	var raw []byte
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT version,created_at,content FROM config_revision WHERE service_id=? AND kind=? AND version=?`, serviceID, kind, version).Scan(&item.Version, &created, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if kind == "env" {
		raw, err = crypto.Decrypt(s.key, raw)
		if err != nil {
			return nil, err
		}
	}
	item.Content, item.CreatedAt = string(raw), time.Unix(created, 0).UTC()
	return &item, nil
}

// RecordEnvRevision snapshots externally managed, editable launcher values.
// Callers must never pass protected launcher keys.
func (s *Store) RecordEnvRevision(ctx context.Context, serviceID int64, content string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.recordEnvRevision(ctx, tx, serviceID, content); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) recordEnvRevision(ctx context.Context, tx *sql.Tx, serviceID int64, content string) error {
	var version int64
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT version,content FROM config_revision WHERE service_id=? AND kind='env' ORDER BY version DESC LIMIT 1`, serviceID).Scan(&version, &raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		plain, err := crypto.Decrypt(s.key, raw)
		if err != nil {
			return err
		}
		if string(plain) == content {
			return nil
		}
	}
	encrypted, err := crypto.Encrypt(s.key, []byte(content))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO config_revision VALUES (?,'env',?,?,?)`, serviceID, version+1, encrypted, time.Now().UTC().Unix())
	return err
}

// Copy complete envfiles without decrypting them: an unreadable environment
// must not prevent the rest of the application from starting. Legacy per-key
// environments are snapshotted when first viewed or replaced.
func (s *Store) backfillEnvRevisions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO config_revision(service_id,kind,version,content,created_at)
        SELECT e.service_id,'env',1,e.content,? FROM service_env e
        JOIN service s ON s.id=e.service_id
        WHERE s.is_self=0 AND NOT EXISTS (
            SELECT 1 FROM config_revision WHERE service_id=e.service_id AND kind='env'
        )`, time.Now().UTC().Unix())
	return err
}
