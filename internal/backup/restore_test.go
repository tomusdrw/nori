package backup

import (
	"context"
	"fmt"
	"os"
	"strings"

	"nori/internal/docker"
	"nori/internal/executor"
	"path/filepath"
	"testing"

	"nori/internal/store"
)

func TestRestoreConfigurationUsesDestinationEncryptionAndManualPolicy(t *testing.T) {
	ctx := context.Background()
	var key [32]byte
	key[0] = 42
	db := filepath.Join(t.TempDir(), "destination.db")
	st, err := store.Open(db, key[:])
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := &Manager{store: st}
	orig := Manifest{
		Service: store.Service{Name: "source", DeploymentMode: store.DeploymentModeManualScript, DeployScript: "true", Policy: store.PolicyScheduled, CronExpr: "0 3 * * *"},
		Policy:  store.BackupPolicy{Keep: 7},
		Env:     "# exact content\nPASSWORD=something\n",
	}
	job, err := m.Restore(ctx, orig, t.TempDir(), RestoreOptions{Name: "restored"})
	if err != nil {
		t.Fatal(err)
	}
	got, env, err := st.BackupSnapshot(ctx, job.ID)
	if err != nil || env != orig.Env || got.Policy != store.PolicyManual || got.CronExpr != "" {
		t.Fatalf("unexpected imported service configuration: %v", err)
	}
	if pending, err := st.RestorePending(ctx, job.ID); pending || err != nil {
		t.Fatalf("unexpected pending import: %v, %v", pending, err)
	}
	if _, err := m.Restore(ctx, orig, t.TempDir(), RestoreOptions{Name: "restored"}); err == nil {
		t.Fatal("restore overwrote an existing service")
	}
}

func TestDiskThresholdsUseBothPercentageAndAbsoluteReserve(t *testing.T) {
	policy := defaultDiskPolicy()
	for _, tc := range []struct {
		available, total int64
		want             string
	}{
		{20 * GiB, 1024 * GiB, "critical"},
		{4 * GiB, 16 * GiB, "warning"},
		{20 * GiB, 100 * GiB, "healthy"},
	} {
		if got := diskLevel(tc.available, tc.total, policy); got != tc.want {
			t.Fatalf("diskLevel(%d, %d) = %s, want %s", tc.available, tc.total, got, tc.want)
		}
	}
}

func TestRestorePreparesManagedPostgresWithRemappedDotenv(t *testing.T) {
	ctx := context.Background()
	dk := &docker.Fake{}
	m, st := newTestManager(t, dk)
	ex := executor.New(st, executor.OSRunner{}, nil, 0)
	ex.SetDocker(dk)
	m.coord = ex
	// Use a Docker CLI stub only for pg_restore and the version query. Actual
	// template lifecycle, environment checks and restore orchestration run.
	bin := t.TempDir()
	dockerCLI := `#!/bin/sh
case " $* " in
  *" psql "*) printf '%s\n' '{"ServerVersion":160005,"Extensions":[]}' ;;
  *" pg_restore "*) cat > "$NORI_TEST_RESTORED_DUMP" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(dockerCLI), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	restored := filepath.Join(dir, "restored-dump")
	t.Setenv("NORI_TEST_RESTORED_DUMP", restored)
	if err := os.WriteFile(filepath.Join(dir, "postgres"), []byte("dump-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Service: store.Service{ID: 42, Name: "source", DeploymentMode: store.DeploymentModePostgres, WatchedImage: "app:1",
			TemplateConfig: `{"version":1,"internal_port":80,"restart_policy":"unless-stopped","health":{"command":"true","timeout_seconds":5},"postgres":{"image":"postgres:16","database":"app","user":"app","password_env":"POSTGRES_PASSWORD","connection_url_env":"DATABASE_URL","ready_timeout_seconds":5}}`},
		Policy:   store.BackupPolicy{Keep: 7},
		Env:      "POSTGRES_DB=app\nPOSTGRES_USER=app\nPOSTGRES_PASSWORD=abc\nDATABASE_URL=postgres://app:abc@nori-42-postgres/app\n",
		Files:    map[string]string{"postgres": "validated-before-restore"},
		Postgres: &PostgresInfo{ServerVersion: 160000},
	}
	svc, err := m.Restore(ctx, manifest, dir, RestoreOptions{Name: "new"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(restored)
	if err != nil || string(data) != "dump-bytes" {
		t.Fatalf("dump not restored: %v", err)
	}
	env, err := st.GetEnvFile(ctx, svc.ID)
	wantURL := fmt.Sprintf("postgres://app:abc@nori-%d-postgres/app", svc.ID)
	if err != nil || !strings.Contains(env, wantURL) {
		t.Fatalf("database endpoint was not remapped: %v", err)
	}
	plan, err := managedPlan(svc)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := dk.ManagedContainers[plan.AppCurrent.Name]; present {
		t.Fatal("restore started the application")
	}
}
