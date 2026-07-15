package store

import (
	"context"
	"strings"
	"testing"
)

func TestEnvVar_SecretEncryptedAtRest(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	secret := "hunter2"
	if err := st.SetEnvVar(ctx, &EnvVar{ServiceID: svc.ID, Key: "DB_PASS", Value: secret, IsSecret: true}); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}

	// Raw column must not equal the plaintext.
	var raw []byte
	if err := st.db.QueryRowContext(ctx, `SELECT value FROM env_var WHERE key='DB_PASS'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) == secret {
		t.Fatal("secret stored in plaintext")
	}

	// Read-back decrypts.
	vars, err := st.ListEnvVars(ctx, svc.ID)
	if err != nil {
		t.Fatalf("ListEnvVars: %v", err)
	}
	if len(vars) != 1 || vars[0].Value != secret {
		t.Fatalf("decrypt mismatch: %+v", vars)
	}
}

func TestEnvVar_NonSecretPlaintext(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "x", Policy: PolicyManual}
	st.CreateService(ctx, svc)
	if err := st.SetEnvVar(ctx, &EnvVar{ServiceID: svc.ID, Key: "PORT", Value: "8080"}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	st.db.QueryRowContext(ctx, `SELECT value FROM env_var WHERE key='PORT'`).Scan(&raw)
	if string(raw) != "8080" {
		t.Fatalf("non-secret should be plaintext, got %q", raw)
	}
}

func TestEnvFile_EncryptedAtRestAndRoundTrips(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "x", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	content := "# production\nDB_PASSWORD=very-secret\nPORT=8080\n"
	if err := st.SetEnvFile(ctx, svc.ID, content); err != nil {
		t.Fatal(err)
	}

	var raw []byte
	if err := st.db.QueryRowContext(ctx, `SELECT content FROM service_env WHERE service_id=?`, svc.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "very-secret") {
		t.Fatal("env file stored in plaintext")
	}

	got, err := st.GetEnvFile(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatalf("GetEnvFile() = %q, want %q", got, content)
	}
}

func TestEnvFile_ReadsLegacyPerVariableRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "legacy", WatchedImage: "x", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	for _, item := range []*EnvVar{
		{ServiceID: svc.ID, Key: "PORT", Value: "8080"},
		{ServiceID: svc.ID, Key: "DB_PASSWORD", Value: "secret value", IsSecret: true},
	} {
		if err := st.SetEnvVar(ctx, item); err != nil {
			t.Fatal(err)
		}
	}

	content, err := st.GetEnvFile(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PORT=8080", "DB_PASSWORD=\"secret value\""} {
		if !strings.Contains(content, want) {
			t.Errorf("converted dotenv file missing %q: %q", want, content)
		}
	}
	if err := st.SetEnvFile(ctx, svc.ID, content); err != nil {
		t.Fatal(err)
	}
	var legacyCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM env_var WHERE service_id=?`, svc.ID).Scan(&legacyCount); err != nil {
		t.Fatal(err)
	}
	if legacyCount != 0 {
		t.Fatalf("legacy env rows were not removed: %d", legacyCount)
	}
}
