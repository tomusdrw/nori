package store

import (
	"context"
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
