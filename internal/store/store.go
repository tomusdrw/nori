package store

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	key []byte
}

const schema = `
CREATE TABLE IF NOT EXISTS service (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	watched_image TEXT NOT NULL,
	policy TEXT NOT NULL,
	cron_expr TEXT NOT NULL DEFAULT '',
	deploy_script TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS env_var (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id INTEGER NOT NULL REFERENCES service(id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	value BLOB NOT NULL,
	is_secret INTEGER NOT NULL DEFAULT 0,
	UNIQUE(service_id, key)
);
CREATE TABLE IF NOT EXISTS deployment (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id INTEGER NOT NULL REFERENCES service(id) ON DELETE CASCADE,
	trigger TEXT NOT NULL,
	target_digest TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	log TEXT NOT NULL DEFAULT ''
);
`

func Open(path string, key []byte) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, key: key}, nil
}

func (s *Store) Close() error { return s.db.Close() }
