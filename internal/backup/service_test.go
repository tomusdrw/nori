package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nori/internal/docker"
	"nori/internal/store"
)

func newTestManager(t *testing.T, dk docker.Client) (*Manager, *store.Store) {
	t.Helper()
	t.Setenv("NORI_BACKUP_DIR", "")
	t.Setenv("NORI_BACKUP_RECIPIENT", "")
	db := filepath.Join(t.TempDir(), "nori.db")
	st, err := store.Open(db, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, err := New(context.Background(), st, dk, nil, db, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SaveDiskPolicy(context.Background(), DiskPolicy{}); err != nil {
		t.Fatal(err)
	}
	return m, st
}

func TestLocalCaptureRetainsLastGoodArchiveAfterExportFailure(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t, nil)
	svc := &store.Service{Name: "app", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript, DeployScript: "true"}
	env := "APP_SECRET=example\n"
	if err := st.SaveServiceConfig(ctx, svc, &env, nil); err != nil {
		t.Fatal(err)
	}
	runCapture := func() store.BackupJob {
		t.Helper()
		if ok, err := m.Queue(ctx, *svc); err != nil || !ok {
			t.Fatalf("queue: %v %v", ok, err)
		}
		jobs, err := st.BackupsByStatus(ctx, "queued")
		if err != nil || len(jobs) != 1 {
			t.Fatalf("queued jobs: %v %v", jobs, err)
		}
		m.capture(ctx, jobs[0])
		job, err := st.BackupJob(ctx, jobs[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	policy := store.BackupPolicy{Keep: 1, Export: `printf '%s' "$APP_SECRET" > "$NORI_BACKUP_WORK/data"`, Import: `test "$(cat "$NORI_BACKUP_WORK/data")" = "$APP_SECRET"`}
	if err := st.SaveBackupPolicy(ctx, svc.ID, policy); err != nil {
		t.Fatal(err)
	}
	expired := runCapture()
	good := runCapture()
	if good.Status != "complete" {
		t.Fatalf("local backup failed: %s", good.Message)
	}
	if _, err := os.Stat(m.archivePath(expired.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected retention to delete %s, got %v", m.archivePath(expired.ID), err)
	}
	policy.Export = "false"
	if err := st.SaveBackupPolicy(ctx, svc.ID, policy); err != nil {
		t.Fatal(err)
	}
	if failed := runCapture(); failed.Status != "failed" {
		t.Fatalf("failed export was marked %s", failed.Status)
	}
	f, err := m.Open(good)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	manifest, dir, cleanup, err := m.DecodeImport(ctx, f, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if manifest.Env != env || manifest.Files["custom"] == "" {
		t.Fatal("archive is missing the configuration or exported data")
	}
	if _, err := m.Restore(ctx, manifest, dir, RestoreOptions{Name: "app-copy"}); err == nil {
		t.Fatal("custom import ran without explicit review")
	}
	if _, err := m.Restore(ctx, manifest, dir, RestoreOptions{Name: "app-copy", ReviewCommands: true}); err != nil {
		t.Fatalf("custom data did not restore: %v", err)
	}
}

func TestBackupRestartsPromotedManagedAppWhenExportFails(t *testing.T) {
	ctx := context.Background()
	dk := &docker.Fake{}
	m, st := newTestManager(t, dk)
	svc := &store.Service{Name: "app", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeSingleContainer, WatchedImage: "nginx:1.27",
		TemplateConfig: `{"version":1,"internal_port":80,"restart_policy":"unless-stopped","health":{"command":"true","timeout_seconds":5}}`}
	if err := st.SaveServiceConfig(ctx, svc, nil, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := managedPlan(*svc)
	if err != nil {
		t.Fatal(err)
	}
	// Managed promotion renames the candidate. Docker keeps its candidate role.
	labels := plan.AppCandidate.Labels
	dk.ManagedContainers = map[string]docker.ManagedContainer{
		plan.AppCurrent.Name: {ManagedContainerSpec: docker.ManagedContainerSpec{Labels: labels}, State: "running"},
	}
	p := store.BackupPolicy{Keep: 7, StopApp: true, Export: "false", Import: "true"}
	if err := st.SaveBackupPolicy(ctx, svc.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Queue(ctx, *svc); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.BackupsByStatus(ctx, "queued")
	m.capture(ctx, jobs[0])
	got := dk.ManagedContainers[plan.AppCurrent.Name]
	if got.State != "running" || !strings.Contains(strings.Join(dk.Operations, "\n"), "stop container "+plan.AppCurrent.Name) {
		t.Fatalf("expected to quiesce and restart the application, got %v", dk.Operations)
	}
	if jobs, err := st.BackupsToResume(ctx); err != nil || len(jobs) != 0 {
		t.Fatalf("resume was not cleared: %v %v", jobs, err)
	}
}

func TestDiskAlertsPersistAndReportRecovery(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t, nil)
	usage, _, err := diskUsage(m.dir, DiskPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	policy := DiskPolicy{WarningBytes: usage.Total + GiB}
	if err := m.SaveDiskPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	var notices []string
	notify := func(_ context.Context, text string) error {
		notices = append(notices, text)
		return nil
	}
	m.notify = notify
	m.monitorDisks(ctx)
	m.monitorDisks(ctx)
	if len(notices) != 1 || !strings.Contains(notices[0], "warning") {
		t.Fatalf("expected one warning, got %v", notices)
	}
	restarted, err := New(ctx, st, nil, nil, filepath.Join(m.dataDir, "nori.db"), "", notify)
	if err != nil {
		t.Fatal(err)
	}
	restarted.monitorDisks(ctx)
	if len(notices) != 1 {
		t.Fatalf("restart repeated the alert: %v", notices)
	}
	if err := restarted.SaveDiskPolicy(ctx, DiskPolicy{}); err != nil {
		t.Fatal(err)
	}
	restarted.monitorDisks(ctx)
	if len(notices) != 2 || !strings.Contains(notices[1], "healthy") {
		t.Fatalf("expected a recovery notice, got %v", notices)
	}
}

func TestScheduleCoalescesMissedRuns(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t, nil)
	svc := &store.Service{Name: "scheduled-copy", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript, DeployScript: "true"}
	if err := st.SaveServiceConfig(ctx, svc, nil, nil); err != nil {
		t.Fatal(err)
	}
	policy, _, err := st.BackupPolicy(ctx, svc.ID)
	if err != nil || policy.Keep != 7 || policy.Schedule == "" {
		t.Fatalf("unexpected default backup policy: %v, %v", policy, err)
	}
	if err := st.InitializeBackupSchedule(ctx, svc.ID, policy, time.Now().Add(-3*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.schedule(ctx); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := st.BackupHistory(ctx, svc.ID)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "queued" {
		t.Fatalf("expected one coalesced catch-up, got %v, %v", jobs, err)
	}
}

func TestInterruptedRestoreCleanupLeavesExistingVolumeAlone(t *testing.T) {
	ctx := context.Background()
	dk := &docker.Fake{Volumes: map[string]docker.ManagedResource{"existing": {Name: "existing"}}}
	m, st := newTestManager(t, dk)
	svc := &store.Service{Name: "unfinished", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript, DeployScript: "true"}
	policy := store.BackupPolicy{Keep: 7, Sources: []store.BackupSource{{Type: "volume", Path: "existing"}}}
	if err := st.SaveRestoredService(ctx, svc, "", policy, "attempt", func(_ int64, value string) (string, error) { return value, nil }); err != nil {
		t.Fatal(err)
	}
	m.recoverRestores(ctx)
	if _, err := st.GetService(ctx, svc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("interrupted service not cleaned: %v", err)
	}
	if _, ok := dk.Volumes["existing"]; !ok {
		t.Fatal("cleanup removed a volume belonging to another service")
	}
}
