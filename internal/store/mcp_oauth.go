package store

import (
	"context"
	"errors"
	"time"
)

const oauthSchema = `CREATE TABLE IF NOT EXISTS mcp_oauth (
 key TEXT PRIMARY KEY, kind TEXT NOT NULL, data BLOB NOT NULL, expires INTEGER NOT NULL,
 family TEXT NOT NULL DEFAULT '', used INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS mcp_oauth_family ON mcp_oauth(family);
 CREATE INDEX IF NOT EXISTS mcp_oauth_expiry ON mcp_oauth(expires);`

type OAuthRecord struct {
	Key, Kind, Family string
	Data              []byte
	Expires           int64
	Used              bool
}

// PutOAuth stores only digests of credentials; the caller supplies serialized metadata.
// The atomic INSERT bounds persistent storage even under simultaneous registration.
func (s *Store) PutOAuth(ctx context.Context, r OAuthRecord) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM mcp_oauth WHERE expires <= ?`, time.Now().Unix()); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO mcp_oauth(key,kind,data,expires,family) SELECT ?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM mcp_oauth WHERE kind='revoked' AND family=?) AND (SELECT count(*) FROM mcp_oauth)<20000 AND (? != 'client' OR (SELECT count(*) FROM mcp_oauth WHERE kind='client')<1000)`, r.Key, r.Kind, r.Data, r.Expires, r.Family, r.Family, r.Kind)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return errors.New("OAuth storage capacity reached")
	}
	return err
}
func (s *Store) GetOAuth(ctx context.Context, key, kind string) (OAuthRecord, error) {
	r := OAuthRecord{Key: key, Kind: kind}
	err := s.db.QueryRowContext(ctx, `SELECT data,expires,family,used FROM mcp_oauth WHERE key=? AND kind=? AND expires>?`, key, kind, time.Now().Unix()).Scan(&r.Data, &r.Expires, &r.Family, &r.Used)
	return r, err
}
func (s *Store) ConsumeOAuth(ctx context.Context, key string) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE mcp_oauth SET used=1 WHERE key=? AND used=0 AND expires>?`, key, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) ExtendOAuthClient(ctx context.Context, key string, expires time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE mcp_oauth SET expires=? WHERE key=? AND kind='client' AND used=0 AND expires>?`, expires.Unix(), key, time.Now().Unix())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrNotFound
	}
	return err
}
func (s *Store) RevokeOAuthFamily(ctx context.Context, family string) error {
	if family == "" {
		return errors.New("empty OAuth family")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A tombstone prevents a concurrent code/refresh exchange from creating
	// new live credentials after another server instance revokes the family.
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO mcp_oauth(key,kind,data,expires,family,used) VALUES (?, 'revoked', '{}', ?, ?, 1)`, "revoked:"+family, time.Now().Add(31*24*time.Hour).Unix(), family); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth SET used=1 WHERE family=?`, family); err != nil {
		return err
	}
	return tx.Commit()
}
