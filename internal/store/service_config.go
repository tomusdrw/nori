package store

import (
	"context"
	"errors"
	"time"

	"deploybot/internal/crypto"
)

var ErrServiceConflict = errors.New("service configuration changed; read it again and retry")

// SaveServiceConfig atomically saves service configuration and, when supplied,
// its encrypted environment. Existing names and managed status are immutable;
// updates require the previous configuration to detect concurrent edits.
func (s *Store) SaveServiceConfig(ctx context.Context, svc *Service, env *string, previous *Service) error {
	var encrypted []byte
	var err error
	if env != nil {
		encrypted, err = crypto.Encrypt(s.key, []byte(*env))
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	id := svc.ID
	if id == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO service (name,watched_image,policy,cron_expr,deploy_script,health_url,is_self,created_at,updated_at) VALUES (?,?,?,?,?,?,0,?,?)`, svc.Name, svc.WatchedImage, svc.Policy, svc.CronExpr, svc.DeployScript, svc.HealthURL, now.Unix(), now.Unix())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
	} else {
		if previous == nil {
			return errors.New("previous service configuration is required for updates")
		}
		res, err := tx.ExecContext(ctx, `UPDATE service SET watched_image=?,policy=?,cron_expr=?,deploy_script=?,health_url=?,updated_at=? WHERE id=? AND name=? AND is_self=0 AND watched_image=? AND policy=? AND cron_expr=? AND deploy_script=? AND health_url=?`, svc.WatchedImage, svc.Policy, svc.CronExpr, svc.DeployScript, svc.HealthURL, now.Unix(), id, svc.Name, previous.WatchedImage, previous.Policy, previous.CronExpr, previous.DeployScript, previous.HealthURL)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrServiceConflict
		}
	}
	if env != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO service_env(service_id,content) VALUES (?,?) ON CONFLICT(service_id) DO UPDATE SET content=excluded.content`, id, encrypted); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM env_var WHERE service_id=?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if svc.ID == 0 {
		svc.CreatedAt = now
	}
	svc.ID, svc.UpdatedAt = id, now
	return nil
}
