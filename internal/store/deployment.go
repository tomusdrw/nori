package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	DeployPending = "pending"
	DeployRunning = "running"
	DeploySuccess = "success"
	DeployFailed  = "failed"
)

const (
	TriggerAuto      = "auto"
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

func (s *Store) CreateDeployment(ctx context.Context, d *Deployment) error {
	now := time.Now().UTC()
	d.StartedAt = now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO deployment (service_id, trigger, target_digest, status, started_at, finished_at, log)
		 VALUES (?,?,?,?,?,?,?)`,
		d.ServiceID, d.Trigger, d.TargetDigest, d.Status, d.StartedAt.Unix(), nil, d.Log)
	if err != nil {
		return err
	}
	d.ID, err = res.LastInsertId()
	return err
}

func (s *Store) UpdateDeployment(ctx context.Context, d *Deployment) error {
	var finished *int64
	if d.FinishedAt != nil {
		v := d.FinishedAt.Unix()
		finished = &v
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE deployment SET status=?, finished_at=?, log=? WHERE id=?`,
		d.Status, finished, d.Log, d.ID)
	return err
}

func (s *Store) GetDeployment(ctx context.Context, id int64) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, service_id, trigger, target_digest, status, started_at, finished_at, log
		 FROM deployment WHERE id=?`, id)
	return scanDeployment(row)
}

func (s *Store) ListDeployments(ctx context.Context, serviceID int64, limit int) ([]*Deployment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, trigger, target_digest, status, started_at, finished_at, log
		 FROM deployment WHERE service_id=? ORDER BY started_at DESC LIMIT ?`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDeployment(sc rowScanner) (*Deployment, error) {
	var d Deployment
	var finished sql.NullInt64
	var started int64
	err := sc.Scan(&d.ID, &d.ServiceID, &d.Trigger, &d.TargetDigest, &d.Status,
		&started, &finished, &d.Log)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.StartedAt = time.Unix(started, 0).UTC()
	if finished.Valid {
		t := time.Unix(finished.Int64, 0).UTC()
		d.FinishedAt = &t
	}
	return &d, nil
}
