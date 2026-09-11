package store

import (
	"context"
	"errors"
)

// GetDeploymentWithLogLimit bounds log reads in SQLite before allocating them
// in the server. A zero limit omits the log entirely.
func (s *Store) GetDeploymentWithLogLimit(ctx context.Context, id int64, limit int) (*Deployment, error) {
	if limit < 0 || limit > 1<<20 {
		return nil, errors.New("invalid deployment log limit")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,service_id,trigger,target_digest,status,started_at,finished_at,
		CASE WHEN ?=0 THEN '' ELSE CAST(substr(CAST(log AS BLOB), -?) AS TEXT) END
		FROM deployment WHERE id=?`, limit, limit, id)
	return scanDeployment(row)
}

func (s *Store) ListDeploymentSummaries(ctx context.Context, serviceID int64, limit int) ([]*Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,service_id,trigger,target_digest,status,started_at,finished_at,''
		FROM deployment WHERE service_id=? ORDER BY started_at DESC LIMIT ?`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
