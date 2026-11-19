package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/robfig/cron/v3"

	"nori/internal/docker"
	"nori/internal/store"
)

type Coordinator interface {
	BeginServiceOperation(context.Context, int64) (func(), error)
}

type Manager struct {
	store        *store.Store
	docker       docker.Client
	coord        Coordinator
	dockerHost   string
	instanceID   string
	dir          string
	dataDir      string
	identity     *age.X25519Identity
	recipient    age.Recipient
	notify       func(context.Context, string) error
	wake         chan struct{}
	restoreSlots chan struct{}
	archiveMu    sync.Mutex
	restoreMu    sync.Mutex
}

const identitySetting = "backup_identity"

func New(ctx context.Context, st *store.Store, dk docker.Client, coord Coordinator, dbPath, dockerHost string, notify func(context.Context, string) error) (*Manager, error) {
	dir, dataDir, err := backupDir(dbPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if notify == nil {
		notify = func(context.Context, string) error { return nil }
	}
	m := &Manager{store: st, docker: dk, coord: coord, dockerHost: dockerHost, dir: dir, dataDir: dataDir,
		notify: notify, wake: make(chan struct{}, 1), restoreSlots: make(chan struct{}, 1)}
	m.instanceID, err = st.GetSetting(ctx, "backup_instance_id")
	if errors.Is(err, store.ErrNotFound) {
		m.instanceID = rand.Text()
		err = st.SetSetting(ctx, "backup_instance_id", m.instanceID)
	}
	if err != nil {
		return nil, err
	}
	if custom := os.Getenv("NORI_BACKUP_RECIPIENT"); custom != "" {
		m.recipient, err = age.ParseX25519Recipient(custom)
		if err != nil {
			return nil, err
		}
		m.reconcile(ctx)
		return m, nil
	}
	raw, err := st.ReadBackupSecret(ctx, identitySetting)
	switch {
	case errors.Is(err, store.ErrNotFound):
		m.identity, err = age.GenerateX25519Identity()
		if err == nil {
			err = st.WriteBackupSecret(ctx, identitySetting, m.identity.String())
		}
	case err == nil:
		m.identity, err = age.ParseX25519Identity(raw)
	}
	if err != nil {
		return nil, err
	}
	m.recipient = m.identity.Recipient()
	m.reconcile(ctx)
	return m, nil
}

func (m *Manager) RecoveryKey() string {
	if m.identity == nil {
		return ""
	}
	return m.identity.String()
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Queue(ctx context.Context, svc store.Service) (bool, error) {
	queued, err := m.store.QueueBackup(ctx, svc.ID)
	if queued {
		m.signal()
	}
	return queued, err
}

func (m *Manager) SavePolicy(ctx context.Context, svc store.Service, policy store.BackupPolicy) error {
	if err := ValidatePolicy(ctx, svc, policy); err != nil {
		return err
	}
	if err := m.store.SaveBackupPolicy(ctx, svc.ID, policy); err != nil {
		return err
	}
	m.prune(ctx, svc.ID)
	m.signal()
	return nil
}

func (m *Manager) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for _, worker := range []func(context.Context){m.runCaptures, m.runUploads, m.runMonitor} {
		workers.Go(func() { worker(ctx) })
	}
	workers.Wait()
}

func (m *Manager) runMonitor(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		m.monitorDisks(ctx)
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (m *Manager) runCaptures(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		m.recoverWriters(ctx)
		m.recoverRestores(ctx)
		if err := m.schedule(ctx); err != nil {
			log.Printf("backup: schedule: %v", err)
		}
		jobs, err := m.store.BackupsByStatus(ctx, "queued")
		if err != nil {
			log.Printf("backup: load queued jobs: %v", err)
		}
		for _, job := range jobs {
			if ctx.Err() != nil {
				return
			}
			m.capture(ctx, job)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		case <-m.wake:
		}
	}
}

func (m *Manager) schedule(ctx context.Context) error {
	services, err := m.store.ListServices(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, svc := range services {
		if svc.IsSelf {
			continue
		}
		policy, last, err := m.store.BackupPolicy(ctx, svc.ID)
		if err != nil {
			return err
		}
		if policy.Schedule == "" {
			continue
		}
		sched, err := cronSchedule(policy.Schedule)
		if err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		if last.IsZero() {
			// Start existing services at their next assigned slot instead of
			// queueing historical runs when this version starts.
			if err := m.store.InitializeBackupSchedule(ctx, svc.ID, policy, now); err != nil {
				return err
			}
			continue
		}
		next := sched.Next(last)
		if !next.IsZero() && !next.After(now) {
			if _, err := m.store.QueueScheduledBackup(ctx, svc.ID, policy, last, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) archivePath(id int64) string {
	return filepath.Join(m.dir, fmt.Sprintf("backup-%d.nori", id))
}

func (m *Manager) Open(job store.BackupJob) (*os.File, error) {
	if job.Status != "complete" {
		return nil, store.ErrNotFound
	}
	return os.Open(m.archivePath(job.ID))
}

func (m *Manager) reconcile(ctx context.Context) {
	if err := m.store.ResetInterruptedUploads(ctx); err != nil {
		log.Printf("backup: reconcile interrupted uploads: %v", err)
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		log.Printf("backup: scan work files: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nori-work-") || strings.HasPrefix(entry.Name(), ".nori-write-") {
			if err := os.RemoveAll(filepath.Join(m.dir, entry.Name())); err != nil {
				log.Printf("backup: remove interrupted staging files: %v", err)
			}
		}
	}
	jobs, err := m.store.BackupsByStatus(ctx, "running")
	if err != nil {
		log.Printf("backup: read interrupted jobs: %v", err)
	}
	for _, job := range jobs {
		_ = os.Remove(m.archivePath(job.ID))
		m.finishJob(ctx, job.ID, "failed", "Interrupted by Nori restart", 0, "")
	}
}

func (m *Manager) capture(parent context.Context, job store.BackupJob) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Hour)
	defer cancel()
	check, err := m.diskGuard(ctx)
	if err == nil {
		err = check()
	}
	if err != nil {
		if ctx.Err() == nil {
			message := "Deferred: could not check free space"
			if errors.Is(err, ErrDiskSpace) {
				message = "Deferred: a monitored filesystem is below the free-space reserve"
			}
			if job.Message != message {
				m.finishJob(ctx, job.ID, "queued", message, 0, "")
			}
		}
		return
	}
	if m.coord != nil {
		release, err := m.coord.BeginServiceOperation(ctx, job.ServiceID)
		if err != nil {
			return
		}
		defer release()
	}
	if err := m.store.SetBackupStatus(ctx, job.ID, "running", "", 0, ""); err != nil {
		log.Printf("backup: claim job: %v", err)
		return
	}
	size, upload, err := m.write(ctx, job, check)
	finish, stop := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer stop()
	if err != nil {
		m.finishJob(finish, job.ID, "failed", err.Error(), 0, "")
		return
	}
	if m.finishJob(finish, job.ID, "complete", "", size, upload) {
		m.prune(finish, job.ServiceID)
	}
}

func (m *Manager) finishJob(ctx context.Context, id int64, status, message string, size int64, upload string) bool {
	if err := m.store.SetBackupStatus(ctx, id, status, message, size, upload); err != nil {
		log.Printf("backup: save job %d state: %v", id, err)
		return false
	}
	return true
}

func (m *Manager) write(ctx context.Context, job store.BackupJob, check func() error) (int64, string, error) {
	svc, env, err := m.store.BackupSnapshot(ctx, job.ServiceID)
	if err != nil {
		return 0, "", errors.New("service configuration could not be read")
	}
	policy, _, err := m.store.BackupPolicy(ctx, job.ServiceID)
	if err != nil {
		return 0, "", errors.New("backup policy could not be read")
	}
	work, err := os.MkdirTemp(m.dir, ".nori-work-")
	if err != nil {
		return 0, "", err
	}
	defer os.RemoveAll(work)
	manifest := Manifest{Service: *svc, Policy: policy, Env: env, Captured: make(map[string]time.Time)}
	if m.docker != nil && svc.WatchedImage != "" {
		if containers, err := m.docker.ListByService(ctx, svc.Name); err == nil {
			running := slices.DeleteFunc(containers, func(c docker.Container) bool { return c.State != "running" })
			manifest.Digest = docker.DigestForImage(running, svc.WatchedImage)
		}
	}
	if err := m.captureData(ctx, job, &manifest, work, check); err != nil {
		return 0, "", err
	}
	for name := range manifest.Captured {
		if name == "postgres" {
			continue
		}
		f, err := os.Open(filepath.Join(work, name))
		if err != nil {
			return 0, "", err
		}
		if err := errors.Join(validateDataTar(f), f.Close()); err != nil {
			return 0, "", fmt.Errorf("cannot safely restore %s: %w", name, err)
		}
	}
	tmp, err := os.CreateTemp(m.dir, ".nori-write-")
	if err != nil {
		return 0, "", err
	}
	defer os.Remove(tmp.Name())
	if err := Encode(checkedWriter{writer: tmp, check: check}, m.recipient, manifest, work); err != nil {
		tmp.Close()
		return 0, "", err
	}
	info, err := tmp.Stat()
	if err := errors.Join(err, tmp.Sync(), tmp.Close()); err != nil {
		return 0, "", err
	}
	if err := os.Rename(tmp.Name(), m.archivePath(job.ID)); err != nil {
		return 0, "", err
	}
	directory, err := os.Open(m.dir)
	if err != nil {
		_ = os.Remove(m.archivePath(job.ID))
		return 0, "", err
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		_ = os.Remove(m.archivePath(job.ID))
		return 0, "", err
	}
	upload := ""
	if !policy.LocalOnly {
		if target, err := m.B2(ctx); err == nil && target.UploadEnabled() {
			upload = "pending"
		}
	}
	return info.Size(), upload, nil
}

func (m *Manager) prune(ctx context.Context, serviceID int64) {
	m.archiveMu.Lock()
	defer m.archiveMu.Unlock()
	policy, _, err := m.store.BackupPolicy(ctx, serviceID)
	if err != nil {
		log.Printf("backup: retention policy: %v", err)
		return
	}
	jobs, err := m.store.RetainedBackups(ctx, serviceID, policy.Keep)
	if err != nil {
		log.Printf("backup: apply retention: %v", err)
		return
	}
	for _, job := range jobs {
		if job.UploadStatus == "uploading" {
			continue
		}
		if err := os.Remove(m.archivePath(job.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("backup: remove expired backup %d: %v", job.ID, err)
			continue
		}
		upload := job.UploadStatus
		if upload == "pending" || upload == "retry" {
			upload = "local archive expired"
		}
		m.finishJob(ctx, job.ID, "expired", "", 0, upload)
	}
}

func (m *Manager) DecodeImport(ctx context.Context, src io.Reader, key string) (Manifest, string, func(), error) {
	select {
	case m.restoreSlots <- struct{}{}:
	default:
		return Manifest{}, "", nil, errors.New("another restore is in progress")
	}
	release := func() { <-m.restoreSlots }
	var identity age.Identity = m.identity
	if key != "" {
		parsed, err := age.ParseX25519Identity(strings.TrimSpace(key))
		if err != nil {
			release()
			return Manifest{}, "", nil, errors.New("invalid recovery key")
		}
		identity = parsed
	} else if m.identity == nil {
		release()
		return Manifest{}, "", nil, errors.New("a recovery key is required")
	}
	check, err := m.diskGuard(ctx)
	if err != nil {
		release()
		return Manifest{}, "", nil, err
	}
	if err := check(); err != nil {
		release()
		return Manifest{}, "", nil, err
	}
	dir, err := os.MkdirTemp(m.dir, ".nori-work-")
	if err != nil {
		release()
		return Manifest{}, "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir); release() }
	manifest, err := decode(src, identity, dir, maxArchiveBytes, check)
	if err != nil {
		cleanup()
		return Manifest{}, "", nil, err
	}
	return manifest, dir, cleanup, nil
}

func cronSchedule(expr string) (cron.Schedule, error) { return cron.ParseStandard(expr) }

func (m *Manager) recoverRestores(ctx context.Context) {
	if !m.restoreMu.TryLock() {
		return
	}
	defer m.restoreMu.Unlock()
	attempts, err := m.store.InterruptedRestores(ctx)
	if err != nil {
		log.Printf("backup: load interrupted restores: %v", err)
		return
	}
	for _, attempt := range attempts {
		cleanup, cancel := context.WithTimeout(ctx, time.Minute)
		err := m.discardRestore(cleanup, attempt)
		cancel()
		if err != nil {
			log.Printf("backup: could not clean up interrupted restore for service %d: %v", attempt.ServiceID, err)
			continue
		}
		log.Printf("backup: cleaned up interrupted restore for service %d", attempt.ServiceID)
	}
}
