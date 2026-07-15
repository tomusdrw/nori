package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

func (s *Store) CreateService(ctx context.Context, svc *Service) error {
	now := time.Now().UTC()
	svc.CreatedAt, svc.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO service (name, watched_image, policy, cron_expr, deploy_script, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)`,
		svc.Name, svc.WatchedImage, string(svc.Policy), svc.CronExpr, svc.DeployScript,
		svc.CreatedAt.Unix(), svc.UpdatedAt.Unix())
	if err != nil {
		return err
	}
	svc.ID, err = res.LastInsertId()
	return err
}

func (s *Store) GetService(ctx context.Context, id int64) (*Service, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id,name,watched_image,policy,cron_expr,deploy_script,created_at,updated_at
		 FROM service WHERE id=?`, id)
	return scanService(row)
}

func (s *Store) ListServices(ctx context.Context) ([]*Service, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,name,watched_image,policy,cron_expr,deploy_script,created_at,updated_at
		 FROM service ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Service
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanService(sc rowScanner) (*Service, error) {
	var svc Service
	var policy string
	var created, updated int64
	err := sc.Scan(&svc.ID, &svc.Name, &svc.WatchedImage, &policy,
		&svc.CronExpr, &svc.DeployScript, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	svc.Policy = Policy(policy)
	svc.CreatedAt = time.Unix(created, 0).UTC()
	svc.UpdatedAt = time.Unix(updated, 0).UTC()
	return &svc, nil
}
