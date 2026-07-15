package config

import (
	"encoding/base64"
	"testing"
)

func validKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("DEPLOYBOT_KEY", validKey())
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
}

func TestLoad_MissingKey(t *testing.T) {
	t.Setenv("DEPLOYBOT_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing DEPLOYBOT_KEY")
	}
}

func TestLoad_BadKeyLength(t *testing.T) {
	t.Setenv("DEPLOYBOT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := Load(); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}
