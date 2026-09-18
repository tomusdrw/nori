package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationRevisions(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "history", WatchedImage: "nginx", Policy: PolicyManual, DeployScript: "echo first"}
	env := "# original\nTOKEN=old-secret\n"
	if err := st.SaveServiceConfig(ctx, svc, &env, nil); err != nil {
		t.Fatal(err)
	}
	before := *svc
	svc.DeployScript = "echo second"
	if err := st.SaveServiceConfigTemplate(ctx, svc, ptrEnv("TOKEN='[REDACTED]'"), &before); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnvSecret(ctx, svc.ID, "TOKEN", "new-secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	scripts, err := st.ListConfigRevisions(ctx, svc.ID, "script")
	if err != nil || len(scripts) != 2 {
		t.Fatalf("scripts = %+v, %v", scripts, err)
	}
	first, err := st.GetConfigRevision(ctx, svc.ID, "script", 1)
	if err != nil || first.Content != "echo first" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	firstEnv, err := st.GetConfigRevision(ctx, svc.ID, "env", 1)
	if err != nil || firstEnv.Content != env {
		t.Fatalf("env round-trip failed: %v", err)
	}
	revisions, err := st.ListConfigRevisions(ctx, svc.ID, "env")
	if err != nil || len(revisions) != 3 {
		t.Fatalf("env versions = %d, %v", len(revisions), err)
	}
	current, _ := st.GetEnvFile(ctx, svc.ID)
	if err := st.SetEnvFile(ctx, svc.ID, current); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnvFile(ctx, svc.ID, env); err != nil {
		t.Fatal(err)
	}
	revisions, err = st.ListConfigRevisions(ctx, svc.ID, "env")
	if err != nil || len(revisions) != 4 || revisions[0].Version != 4 {
		t.Fatalf("restore/no-op versions = %+v, %v", revisions, err)
	}
	var raw []byte
	if err := st.db.QueryRow(`SELECT content FROM config_revision WHERE service_id=? AND kind='env' AND version=1`, svc.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "old-secret") {
		t.Fatal("history contains plaintext secret")
	}
	if _, err := st.GetConfigRevision(ctx, svc.ID+1, "env", 1); err != ErrNotFound {
		t.Fatalf("cross-service lookup: %v", err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_history BEFORE INSERT ON config_revision BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	before = *svc
	svc.DeployScript = "echo failed"
	if err := st.SaveServiceConfig(ctx, svc, ptrEnv("TOKEN=failed"), &before); err == nil {
		t.Fatal("history failure should abort save")
	}
	saved, _ := st.GetService(ctx, svc.ID)
	if saved.DeployScript != "echo second" {
		t.Fatal("failed save changed script")
	}
	if err := st.DeleteService(ctx, svc.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT count(*) FROM config_revision`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan history: %d, %v", count, err)
	}
}

func TestRevisionMigrationAndRestart(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	svc := &Service{Name: "legacy-history", WatchedImage: "image", Policy: PolicyManual, DeployScript: "echo legacy"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnvVar(ctx, &EnvVar{ServiceID: svc.ID, Key: "TOKEN", Value: "legacy-secret", IsSecret: true}); err != nil {
		t.Fatal(err)
	}
	// Simulate a database from before history was introduced.
	if _, err := st.db.Exec(`DELETE FROM config_revision`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(revisionSchema); err != nil {
		t.Fatal(err)
	}
	if err := st.backfillEnvRevisions(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.db.Exec(revisionSchema); err != nil {
			t.Fatal(err)
		}
		if err := st.backfillEnvRevisions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	scripts, err := st.ListConfigRevisions(ctx, svc.ID, "script")
	if err != nil || len(scripts) != 1 {
		t.Fatalf("backfill scripts: %+v %v", scripts, err)
	}
	// Legacy values are captured before the first replacement, not decrypted at startup.
	if err := st.SetEnvFile(ctx, svc.ID, ""); err != nil {
		t.Fatal(err)
	}
	env, err := st.GetConfigRevision(ctx, svc.ID, "env", 1)
	if err != nil || !strings.Contains(env.Content, "legacy-secret") {
		t.Fatalf("backfill env: %v", err)
	}
	if err := st.SetEnvFile(ctx, svc.ID, ""); err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListConfigRevisions(ctx, svc.ID, "env")
	if err != nil || len(versions) != 2 {
		t.Fatalf("clear env: %+v %v", versions, err)
	}
	cleared, err := st.GetConfigRevision(ctx, svc.ID, "env", 2)
	if err != nil || cleared.Content != "" {
		t.Fatalf("empty snapshot: %+v %v", cleared, err)
	}
}

func TestEnvironmentHistoryFailureRollsBackScriptAndEnvironment(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "atomic-history", WatchedImage: "image", Policy: PolicyManual, DeployScript: "echo original"}
	env := "TOKEN=original"
	if err := st.SaveServiceConfig(ctx, svc, &env, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_env_history BEFORE INSERT ON config_revision WHEN NEW.kind='env' BEGIN SELECT RAISE(ABORT, 'history failed'); END`); err != nil {
		t.Fatal(err)
	}
	before := *svc
	svc.DeployScript = "echo changed"
	if err := st.SaveServiceConfig(ctx, svc, ptrEnv("TOKEN=changed"), &before); err == nil {
		t.Fatal("expected history failure")
	}
	saved, err := st.GetService(ctx, svc.ID)
	if err != nil || saved.DeployScript != before.DeployScript {
		t.Fatal("script did not roll back")
	}
	got, err := st.GetEnvFile(ctx, svc.ID)
	if err != nil || got != env {
		t.Fatal("env did not roll back")
	}
	for _, kind := range []string{"script", "env"} {
		versions, err := st.ListConfigRevisions(ctx, svc.ID, kind)
		if err != nil || len(versions) != 1 {
			t.Fatalf("phantom %s version: %+v %v", kind, versions, err)
		}
	}
}

func TestRevisionBackfillDoesNotDecryptAtStartup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "existing.db")
	key := make([]byte, 32)
	st, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Name: "corrupt-env", WatchedImage: "image", Policy: PolicyManual, DeployScript: "echo ok"}
	if err := st.SaveServiceConfig(ctx, svc, ptrEnv("TOKEN=old"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE service_env SET content=x'00'; DELETE FROM config_revision WHERE kind='env'`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path, key)
	if err != nil {
		t.Fatalf("one unreadable environment prevented startup: %v", err)
	}
	defer st.Close()
	if _, err := st.GetService(ctx, svc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetEnvFile(ctx, svc.ID); err == nil {
		t.Fatal("corrupt env unexpectedly readable")
	}
	if err := st.CreateService(ctx, &Service{Name: "healthy", WatchedImage: "image", Policy: PolicyManual}); err != nil {
		t.Fatalf("other service unavailable: %v", err)
	}
	versions, err := st.ListConfigRevisions(ctx, svc.ID, "env")
	if err != nil || len(versions) != 1 {
		t.Fatalf("ciphertext baseline missing: %+v %v", versions, err)
	}
}
