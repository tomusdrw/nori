package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
)

const DefaultServiceGroup = "Ungrouped"

const serviceGroupSchema = `
CREATE TABLE IF NOT EXISTS service_group (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL COLLATE NOCASE UNIQUE,
	position INTEGER NOT NULL,
	collapsed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS service_group_member (
	service_id INTEGER PRIMARY KEY REFERENCES service(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL REFERENCES service_group(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS service_group_members_by_group ON service_group_member(group_id);
INSERT INTO service_group (name, position) VALUES ('` + DefaultServiceGroup + `', -1)
ON CONFLICT(name) DO UPDATE SET name = excluded.name WHERE service_group.name COLLATE BINARY <> excluded.name;
`

const activeServiceGroups = `SELECT g.id, g.name, g.collapsed, g.position FROM service_group g
	WHERE g.name = '` + DefaultServiceGroup + `' OR EXISTS(SELECT 1 FROM service_group_member WHERE group_id = g.id)
	ORDER BY g.position, g.id`

type ServiceGroup struct {
	ID        int64
	Name      string
	Collapsed bool
	position  int64
}

type serviceGroupQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) ListServiceGroups(ctx context.Context) ([]ServiceGroup, error) {
	return listServiceGroups(ctx, s.db)
}

func listServiceGroups(ctx context.Context, db serviceGroupQueryer) ([]ServiceGroup, error) {
	rows, err := db.QueryContext(ctx, activeServiceGroups)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []ServiceGroup
	for rows.Next() {
		var group ServiceGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.Collapsed, &group.position); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// An absent membership puts the service in the default group. Keep the
// group rows when empty so their presentation settings survive reassignment.
func setServiceGroup(ctx context.Context, tx *sql.Tx, serviceID int64, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, DefaultServiceGroup) {
		_, err := tx.ExecContext(ctx, `DELETE FROM service_group_member WHERE service_id = ?`, serviceID)
		return DefaultServiceGroup, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO service_group (name, position)
		SELECT ?, COALESCE(MAX(position), 0) + 1 FROM service_group WHERE true
		ON CONFLICT(name) DO NOTHING`, name); err != nil {
		return "", err
	}
	var canonicalName string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO service_group_member (service_id, group_id)
		SELECT ?, id FROM service_group WHERE name = ?
		ON CONFLICT(service_id) DO UPDATE SET group_id = excluded.group_id
		RETURNING (SELECT name FROM service_group WHERE id = group_id)`, serviceID, name).Scan(&canonicalName)
	return canonicalName, err
}

// MoveServiceGroup places id immediately before or after another visible group.
func (s *Store) MoveServiceGroup(ctx context.Context, id, targetID int64, before bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	groups, err := listServiceGroups(ctx, tx)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	from, target := slices.Index(ids, id), slices.Index(ids, targetID)
	if from < 0 || target < 0 {
		return ErrNotFound
	}
	if from == target {
		return nil
	}
	ids = slices.Delete(ids, from, from+1)
	if from < target {
		target--
	}
	if !before {
		target++
	}
	ids = slices.Insert(ids, target, id)
	for i, group := range groups {
		if ids[i] == group.ID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE service_group SET position = ? WHERE id = ?`, group.position, ids[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetServiceGroupCollapsed(ctx context.Context, id int64, collapsed bool) error {
	err := s.db.QueryRowContext(ctx, `UPDATE service_group SET collapsed = ? WHERE id = ? RETURNING id`, collapsed, id).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
