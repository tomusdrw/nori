package backup

import (
	"archive/tar"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"nori/internal/crypto"
	"nori/internal/deploytemplate"
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
	m := &Manager{store: st, dir: filepath.Dir(db)}
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
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var encrypted []byte
	if err := raw.QueryRow("SELECT content FROM service_env WHERE service_id=?", job.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	decoded, err := crypto.Decrypt(key[:], encrypted)
	if err != nil || string(encrypted) == env || string(decoded) != env {
		t.Fatalf("restored dotenv is not encrypted with the destination key: %v", err)
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

func TestRestoreVolumeCollisionIdentifiesDestination(t *testing.T) {
	dk := &docker.Fake{Volumes: map[string]docker.ManagedResource{"occupied": {Name: "occupied"}}}
	m := &Manager{docker: dk}
	policy := store.BackupPolicy{Sources: []store.BackupSource{
		{Type: "volume", Path: "available"},
		{Type: "volume", Path: "occupied"},
	}}
	err := m.preflightRestore(context.Background(), store.Service{}, "", deploytemplate.Config{}, policy)
	var fieldError *RestoreFieldError
	if !errors.As(err, &fieldError) || fieldError.Field != "target-1" || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("expected error for second target, got %v", err)
	}
}

func TestRestoreRejectsVolumeAliasingManagedDestination(t *testing.T) {
	config := deploytemplate.Config{Volumes: []deploytemplate.VolumeMount{{Name: "files"}}, Postgres: &deploytemplate.PostgresConfig{}}
	for _, name := range []string{"nori-42-volume-files", "nori-42-volume-postgres-data"} {
		err := validateRestoreDestinations(42, config, []store.BackupSource{{Type: "volume", Path: name}})
		var field *RestoreFieldError
		if !errors.As(err, &field) || field.Field != "target-0" {
			t.Fatalf("alias %s was not rejected: %v", name, err)
		}
	}
}

func TestRestoreChecksDiskBeforePull(t *testing.T) {
	ctx := context.Background()
	dk := &docker.Fake{}
	m, _ := newTestManager(t, dk)
	usage, _, err := diskUsage(m.dir, DiskPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	reserve := usage.Total + GiB
	if err := m.SaveDiskPolicy(ctx, DiskPolicy{CriticalBytes: reserve, WarningBytes: reserve}); err != nil {
		t.Fatal(err)
	}
	err = m.preflightRestore(ctx, store.Service{WatchedImage: "nginx:stable"}, "", deploytemplate.Config{}, store.BackupPolicy{})
	if !errors.Is(err, ErrDiskSpace) || len(dk.Operations) != 0 {
		t.Fatalf("preflight pulled despite disk reserve: %v, %v", dk.Operations, err)
	}
}

func TestRestorePostgresTemplateVolumeWithoutDatabaseCredentials(t *testing.T) {
	ctx := context.Background()
	dk := &docker.Fake{}
	m, st := newTestManager(t, dk)
	ex := executor.New(st, executor.OSRunner{}, nil, 0)
	ex.SetDocker(dk)
	m.coord = ex

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	payload, err := os.Create(filepath.Join(dir, "source-0"))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(payload)
	if err := errors.Join(archive.Close(), payload.Close()); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Service: store.Service{ID: 42, Name: "original", DeploymentMode: store.DeploymentModePostgres, WatchedImage: "app:1",
			TemplateConfig: `{"version":1,"internal_port":80,"restart_policy":"unless-stopped","health":{"command":"true","timeout_seconds":5},"volumes":[{"name":"files","target":"/srv/files"}],"postgres":{"image":"postgres:16","database":"app","user":"app","password_env":"POSTGRES_PASSWORD","connection_url_env":"DATABASE_URL","ready_timeout_seconds":5}}`},
		Policy: store.BackupPolicy{Keep: 7, SkipPostgres: true, Sources: []store.BackupSource{{Type: "managed", Path: "files"}}},
		Files:  map[string]string{"source-0": "validated-before-restore"},
	}
	svc, err := m.Restore(ctx, manifest, dir, RestoreOptions{Name: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dk.ManagedContainers) != 0 || len(dk.Volumes) != 1 {
		t.Fatalf("unexpected resources: %v", dk.Operations)
	}
	if _, ok := dk.Volumes[fmt.Sprintf("nori-%d-volume-files", svc.ID)]; !ok {
		t.Fatal("managed application volume was not restored")
	}
}
