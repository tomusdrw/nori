package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"nori/internal/crypto"
)

const backupSchema = `
CREATE TABLE IF NOT EXISTS backup_policy (
	service_id INTEGER PRIMARY KEY REFERENCES service(id) ON DELETE CASCADE,
	config TEXT NOT NULL,
	last_scheduled INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS backup_restore (
	service_id INTEGER PRIMARY KEY REFERENCES service(id) ON DELETE CASCADE,
	token TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS service_backup (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id INTEGER NOT NULL,
	service_name TEXT NOT NULL,
	created INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'queued',
	message TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL DEFAULT 0,
	upload_status TEXT NOT NULL DEFAULT '',
	upload_attempts INTEGER NOT NULL DEFAULT 0,
	next_upload INTEGER NOT NULL DEFAULT 0,
	resume TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS active_service_backup ON service_backup(service_id)
	WHERE status IN ('queued', 'running');
`

type BackupSource struct {
	Type string
	Path string
}

type BackupPolicy struct {
	Schedule     string
	Keep         int
	LocalOnly    bool
	SkipPostgres bool
	Sources      []BackupSource
	StopApp      bool
	Quiesce      string
	Resume       string
	Export       string
	Import       string
}

func DefaultBackupPolicy(id int64) BackupPolicy {
	return BackupPolicy{Schedule: fmt.Sprintf("%d %d * * *", id%60, (id/60)%24), Keep: 7}
}

type BackupJob struct {
	ID             int64
	ServiceID      int64
	ServiceName    string
	Created        time.Time
	Status         string
	Message        string
	Size           int64
	UploadStatus   string
	UploadAttempts int
	NextUpload     int64
	Resume         string
}

func (s *Store) BackupPolicy(ctx context.Context, id int64) (BackupPolicy, time.Time, error) {
	return readBackupPolicy(ctx, s.db.QueryRowContext, id)
}

func readBackupPolicy(ctx context.Context, query func(context.Context, string, ...any) *sql.Row, id int64) (BackupPolicy, time.Time, error) {
	var raw string
	var last int64
	err := query(ctx, `SELECT config,last_scheduled FROM backup_policy WHERE service_id=?`, id).Scan(&raw, &last)
	p := DefaultBackupPolicy(id)
	if errors.Is(err, sql.ErrNoRows) {
		return p, time.Time{}, nil
	}
	if err != nil {
		return p, time.Time{}, err
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, time.Time{}, err
	}
	return p, time.Unix(last, 0), nil
}

func (s *Store) SaveBackupPolicy(ctx context.Context, id int64, policy BackupPolicy) error {
	return saveBackupPolicy(ctx, s.db.ExecContext, id, policy)
}

// SaveBackupPolicyIfUnchanged guards a partial update against concurrent
// policy edits and service changes that can invalidate selected sources.
func (s *Store) SaveBackupPolicyIfUnchanged(ctx context.Context, svc Service, previous, policy BackupPolicy) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT config_version FROM service WHERE id=?`, svc.ID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrServiceConflict
	}
	if err != nil {
		return err
	}
	if version != svc.ConfigVersion {
		return ErrServiceConflict
	}
	current, _, err := readBackupPolicy(ctx, tx.QueryRowContext, svc.ID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, previous) {
		return errors.New("backup policy changed; read it again and retry")
	}
	if reflect.DeepEqual(current, policy) {
		return nil
	}
	if err := saveBackupPolicy(ctx, tx.ExecContext, svc.ID, policy); err != nil {
		return err
	}
	return tx.Commit()
}

func saveBackupPolicy(ctx context.Context, exec func(context.Context, string, ...any) (sql.Result, error), id int64, policy BackupPolicy) error {
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	result, err := exec(ctx, `INSERT INTO backup_policy(service_id,config,last_scheduled)
	SELECT id,?,? FROM service WHERE id=? AND is_self=0 AND
		NOT EXISTS(SELECT 1 FROM backup_restore WHERE service_id=service.id)
	ON CONFLICT(service_id) DO UPDATE SET config=excluded.config,last_scheduled=excluded.last_scheduled`, string(raw), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return errors.New("backup policy cannot change during an incomplete restore")
	}
	return err
}

func (s *Store) BackupSnapshot(ctx context.Context, id int64) (*Service, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	service, err := scanService(tx.QueryRowContext(ctx, `SELECT `+serviceColumns+` FROM service WHERE id=? AND is_self=0`, id))
	if err != nil {
		return nil, "", err
	}
	env, err := s.getEnvFile(ctx, tx, id)
	return service, env, err
}

const queueBackup = `INSERT INTO service_backup(service_id,service_name,created)
	SELECT id,name,? FROM service WHERE id=? AND is_self=0 AND
	NOT EXISTS (SELECT 1 FROM backup_restore WHERE service_id=service.id) ON CONFLICT DO NOTHING`

func (s *Store) QueueBackup(ctx context.Context, id int64) (bool, error) {
	jobID, err := s.QueueBackupID(ctx, id)
	return jobID != 0, err
}

// QueueBackupID returns zero when an active backup or incomplete restore
// prevents the insert.
func (s *Store) QueueBackupID(ctx context.Context, id int64) (int64, error) {
	var jobID int64
	err := s.db.QueryRowContext(ctx, queueBackup+` RETURNING id`, time.Now().Unix(), id).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return jobID, err
}

func (s *Store) QueueScheduledBackup(ctx context.Context, id int64, policy BackupPolicy, last, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	raw, err := json.Marshal(policy)
	if err != nil {
		return false, err
	}
	advanced, err := tx.ExecContext(ctx, `UPDATE backup_policy SET last_scheduled=?
		WHERE service_id=? AND config=? AND last_scheduled=?`, now.Unix(), id, string(raw), last.Unix())
	if err != nil {
		return false, err
	}
	count, err := advanced.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	result, err := tx.ExecContext(ctx, queueBackup, now.Unix(), id)
	if err != nil {
		return false, err
	}
	count, err = result.RowsAffected()
	if err != nil {
		return false, err
	}
	return count == 1, tx.Commit()
}

const backupColumns = `id,service_id,service_name,created,status,message,size,upload_status,upload_attempts,next_upload,resume`

func (s *Store) BackupJob(ctx context.Context, id int64) (BackupJob, error) {
	j, err := scanBackupJob(s.db.QueryRowContext(ctx, `SELECT `+backupColumns+` FROM service_backup WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

func (s *Store) BackupHistory(ctx context.Context, id int64) ([]BackupJob, error) {
	return s.backupJobs(ctx, `WHERE service_id=? ORDER BY id DESC LIMIT 100`, id)
}

func (s *Store) BackupsByStatus(ctx context.Context, status string) ([]BackupJob, error) {
	return s.backupJobs(ctx, `WHERE status=? ORDER BY id`, status)
}

func (s *Store) RetainedBackups(ctx context.Context, id int64, keep int) ([]BackupJob, error) {
	return s.backupJobs(ctx, `WHERE service_id=? AND status='complete' ORDER BY id DESC LIMIT -1 OFFSET ?`, id, keep)
}

func (s *Store) PendingUploads(ctx context.Context) ([]BackupJob, error) {
	return s.backupJobs(ctx, `WHERE status='complete' AND upload_status IN ('pending','retry') AND next_upload<=? ORDER BY id LIMIT 20`, time.Now().Unix())
}

func (s *Store) BackupsToResume(ctx context.Context) ([]BackupJob, error) {
	return s.backupJobs(ctx, `WHERE resume<>'' ORDER BY id`)
}

func (s *Store) backupJobs(ctx context.Context, suffix string, args ...any) ([]BackupJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupColumns+` FROM service_backup `+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []BackupJob
	for rows.Next() {
		job, err := scanBackupJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func scanBackupJob(row rowScanner) (BackupJob, error) {
	var job BackupJob
	var created int64
	err := row.Scan(&job.ID, &job.ServiceID, &job.ServiceName, &created, &job.Status, &job.Message, &job.Size,
		&job.UploadStatus, &job.UploadAttempts, &job.NextUpload, &job.Resume)
	job.Created = time.Unix(created, 0)
	return job, err
}

func (s *Store) SetBackupStatus(ctx context.Context, id int64, status, message string, size int64, uploadStatus string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE service_backup SET status=?,message=?,size=?,upload_status=? WHERE id=?`, status, message, size, uploadStatus, id)
	return err
}

func (s *Store) SetBackupResume(ctx context.Context, id int64, resume string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE service_backup SET resume=? WHERE id=?`, resume, id)
	return err
}

func (s *Store) SetBackupUpload(ctx context.Context, id int64, status string, attempts int, next int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE service_backup SET upload_status=?,upload_attempts=?,next_upload=? WHERE id=? AND status='complete'`, status, attempts, next, id)
	return err
}

func (s *Store) RequeueBackupUpload(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE service_backup SET upload_status='pending', upload_attempts=0, next_upload=0
		WHERE id=? AND status='complete' AND upload_status<>'uploading'`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return errors.New("backup is no longer available for upload")
	}
	return err
}

func (s *Store) WriteBackupSecret(ctx context.Context, name, value string) error {
	sealed, err := crypto.Encrypt(s.key, []byte(value))
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, name, base64.StdEncoding.EncodeToString(sealed))
}

func (s *Store) ReadBackupSecret(ctx context.Context, name string) (string, error) {
	sealed, err := s.GetSetting(ctx, name)
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	data, err := crypto.Decrypt(s.key, ciphertext)
	return string(data), err
}

func (s *Store) InitializeBackupSchedule(ctx context.Context, id int64, policy BackupPolicy, now time.Time) error {
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO backup_policy(service_id,config,last_scheduled)
	SELECT id,?,? FROM service WHERE id=? AND is_self=0 ON CONFLICT DO NOTHING`, string(raw), now.Unix(), id)
	return err
}

func (s *Store) ResetInterruptedUploads(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE service_backup SET upload_status='retry',next_upload=0
	WHERE status='complete' AND upload_status='uploading'`)
	return err
}

func (s *Store) DeleteBackupSecret(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM setting WHERE key=?`, name)
	return err
}

func (s *Store) BackupResumePending(ctx context.Context, id int64) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM service_backup WHERE service_id=? AND resume<>'')`, id).Scan(&pending)
	return pending, err
}

func (s *Store) SaveRestoredService(ctx context.Context, svc *Service, env string, policy BackupPolicy, token string, adjust func(int64, string) (string, error)) error {
	if svc.ID != 0 || svc.IsSelf || svc.Policy != PolicyManual {
		return errors.New("restore requires a new manually deployed service")
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return s.saveServiceConfig(ctx, svc, &env, nil, false, func(tx *sql.Tx, id int64, dotenv string) (string, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO backup_restore(service_id,token) VALUES (?,?)`, id, token); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO backup_policy(service_id,config,last_scheduled) VALUES (?,?,?)`, id, string(raw), time.Now().Unix()); err != nil {
			return "", err
		}
		return adjust(id, dotenv)
	})
}

func (s *Store) RestorePending(ctx context.Context, id int64) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_restore WHERE service_id=?)`, id).Scan(&pending)
	return pending, err
}

func (s *Store) FinishRestore(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backup_restore WHERE service_id=?`, id)
	return err
}

func (s *Store) RecentBackups(ctx context.Context) ([]BackupJob, error) {
	return s.backupJobs(ctx, `ORDER BY id DESC LIMIT 100`)
}

type RestoreAttempt struct {
	ServiceID int64
	Token     string
}

func (s *Store) InterruptedRestores(ctx context.Context) ([]RestoreAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT service_id,token FROM backup_restore ORDER BY service_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []RestoreAttempt
	for rows.Next() {
		var attempt RestoreAttempt
		if err := rows.Scan(&attempt.ServiceID, &attempt.Token); err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) DeleteIncompleteRestore(ctx context.Context, id int64, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM service WHERE id=? AND
		EXISTS(SELECT 1 FROM backup_restore WHERE service_id=? AND token=?)`, id, id, token)
	return err
}
