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
	is_self INTEGER NOT NULL DEFAULT 0,
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
CREATE TABLE IF NOT EXISTS service_env (
	service_id INTEGER PRIMARY KEY REFERENCES service(id) ON DELETE CASCADE,
	content BLOB NOT NULL
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
CREATE TABLE IF NOT EXISTS setting (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
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
	if _, err := db.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, key: key}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(service)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasSelf := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == "is_self" {
			hasSelf = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasSelf {
		if _, err := db.Exec(`ALTER TABLE service ADD COLUMN is_self INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS service_one_self ON service(is_self) WHERE is_self=1`)
	return err
}
