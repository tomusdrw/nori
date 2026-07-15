package config

import (
	"encoding/base64"
	"testing"

	"deploybot/internal/auth"
)

func validKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DEPLOYBOT_KEY", validKey())
	t.Setenv("DEPLOYBOT_SESSION_KEY", validKey())
	hash, err := auth.HashPassword("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEPLOYBOT_ADMIN_HASH", hash)
}

func TestLoad_Defaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if len(cfg.EncryptionKey) != 32 {
		t.Errorf("key len = %d, want 32", len(cfg.EncryptionKey))
	}
	if cfg.TerminalDir != "." {
		t.Errorf("TerminalDir = %q, want .", cfg.TerminalDir)
	}
}

func TestLoad_TerminalDir(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEPLOYBOT_TERMINAL_DIR", "/srv/deploybot")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TerminalDir != "/srv/deploybot" {
		t.Errorf("TerminalDir = %q", cfg.TerminalDir)
	}
}

func TestLoad_MissingKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEPLOYBOT_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing DEPLOYBOT_KEY")
	}
}

func TestLoad_BadKeyLength(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEPLOYBOT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := Load(); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}
