package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nori/internal/docker"
	"nori/internal/executor"
	"nori/internal/store"
)

func TestDockerBackupRestoresPostgresAndManagedVolume(t *testing.T) {
	host := os.Getenv("NORI_BACKUP_TEST_DOCKER")
	if host == "" {
		t.Skip("set NORI_BACKUP_TEST_DOCKER to an isolated Docker-compatible daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dk, err := docker.New(host)
	if err != nil {
		t.Fatal(err)
	}
	newInstance := func(dbPath string) (*Manager, *store.Store, *executor.Executor) {
		t.Helper()
		db, err := store.Open(dbPath, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		// Use distinct resource names even when the test is pointed at a
		// workstation running other tests.
		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`INSERT INTO sqlite_sequence(name,seq) VALUES ('service',?)`, binary.LittleEndian.Uint64(random[:])&((1<<40)-1)); err != nil {
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		ex := executor.New(db, executor.OSRunner{}, nil, 0)
		ex.SetDocker(dk)
		manager, err := New(ctx, db, dk, ex, dbPath, host, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.SaveDiskPolicy(ctx, DiskPolicy{}); err != nil {
			t.Fatal(err)
		}
		return manager, db, ex
	}
	t.Setenv("NORI_BACKUP_DIR", "")
	t.Setenv("NORI_BACKUP_RECIPIENT", "")
	src, db, ex := newInstance(filepath.Join(t.TempDir(), "nori.db"))
	dest, _, _ := newInstance(filepath.Join(t.TempDir(), "nori.db"))
	svc := &store.Service{
		Name:   "backup-it-" + strings.ToLower(rand.Text()[:8]),
		Policy: store.PolicyManual, DeploymentMode: store.DeploymentModePostgres, WatchedImage: "docker.io/library/alpine:3.21",
		TemplateConfig: `{"version":1,"internal_port":80,"restart_policy":"unless-stopped","health":{"command":"true","timeout_seconds":5},"volumes":[{"name":"files","target":"/srv/files"}],"postgres":{"image":"docker.io/library/postgres:16-alpine","database":"app","user":"app","password_env":"POSTGRES_PASSWORD","connection_url_env":"DATABASE_URL","ready_timeout_seconds":45}}`,
	}
	if err := db.SaveServiceConfig(ctx, svc, nil, nil); err != nil {
		t.Fatal(err)
	}
	dotenv := fmt.Sprintf("POSTGRES_DB=app\nPOSTGRES_USER=app\nPOSTGRES_PASSWORD=test-only\nDATABASE_URL=postgres://app:test-only@nori-%d-postgres/app\n", svc.ID)
	before := *svc
	if err := db.SaveServiceConfig(ctx, svc, &dotenv, &before); err != nil {
		t.Fatal(err)
	}
	plan, err := managedPlan(*svc)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func(m *Manager, svc store.Service) {
		t.Helper()
		plan, err := managedPlan(svc)
		if err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = dk.RemoveContainer(ctx, plan.Database.Name)
		for _, v := range plan.Volumes {
			_ = m.dockerCommand(ctx, nil, nil, "volume", "rm", v.Name)
		}
		_ = m.dockerCommand(ctx, nil, nil, "network", "rm", plan.InternalNetwork.Name)
	}
	t.Cleanup(func() { cleanup(src, *svc) })
	if err := ex.PrepareRestore(ctx, svc, plan, dotenv); err != nil {
		t.Fatal(err)
	}
	policy := store.BackupPolicy{Keep: 7, Sources: []store.BackupSource{{Type: "managed", Path: "files"}}}
	if err := db.SaveBackupPolicy(ctx, svc.ID, policy); err != nil {
		t.Fatal(err)
	}
	if err := src.postgresCommand(ctx, plan, nil, nil, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-c", "CREATE TABLE sample(value text); INSERT INTO sample VALUES ('backup retained this');"); err != nil {
		t.Fatal(err)
	}
	volume := fmt.Sprintf("nori-%d-volume-files", svc.ID)
	if err := src.dockerCommand(ctx, nil, nil, "run", "--rm", "--mount", "type=volume,src="+volume+",dst=/files", "docker.io/library/alpine:3.21",
		"sh", "-ec", "echo original > /files/document"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Queue(ctx, *svc); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.BackupsByStatus(ctx, "queued")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("queued jobs: %v %v", jobs, err)
	}
	src.capture(ctx, jobs[0])
	job, err := db.BackupJob(ctx, jobs[0].ID)
	if err != nil || job.Status != "complete" {
		t.Fatalf("archive did not complete: %s %s %v", job.Status, job.Message, err)
	}
	file, err := src.Open(job)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	manifest, dir, removeStage, err := dest.DecodeImport(ctx, file, src.RecoveryKey())
	if err != nil {
		t.Fatal(err)
	}
	defer removeStage()
	restored, err := dest.Restore(ctx, manifest, dir, RestoreOptions{Name: svc.Name + "-copy"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanup(dest, restored) })
	targetPlan, err := managedPlan(restored)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := dest.postgresCommand(ctx, targetPlan, nil, &output, "psql", "-Atc", "SELECT value FROM sample"); err != nil || strings.TrimSpace(output.String()) != "backup retained this" {
		t.Fatalf("restored PostgreSQL rows: %q, %v", output.String(), err)
	}
	output.Reset()
	targetVol := fmt.Sprintf("nori-%d-volume-files", restored.ID)
	if err := dest.dockerCommand(ctx, nil, &output, "run", "--rm", "--mount", "type=volume,src="+targetVol+",dst=/files,readonly", "docker.io/library/alpine:3.21",
		"cat", "/files/document"); err != nil || strings.TrimSpace(output.String()) != "original" {
		t.Fatalf("restored Docker volume: %q, %v", output.String(), err)
	}
}
