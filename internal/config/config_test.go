package config

import (
	"encoding/base64"
	"testing"
	"time"

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

func TestLoad_TwilioDisabledByDefault(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Twilio.Enabled() {
		t.Fatal("Twilio must be disabled when env vars are absent")
	}
}

func TestLoad_TwilioEnabledWhenFullyConfigured(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEPLOYBOT_TWILIO_ACCOUNT_SID", "AC123")
	t.Setenv("DEPLOYBOT_TWILIO_AUTH_TOKEN", "tok")
	t.Setenv("DEPLOYBOT_TWILIO_FROM", "+15551234567")
	t.Setenv("DEPLOYBOT_TWILIO_TO", "+15559876543")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Twilio.Enabled() {
		t.Fatal("Twilio must be enabled when all four vars are set")
	}
	if cfg.Twilio.AccountSID != "AC123" || cfg.Twilio.From != "+15551234567" {
		t.Errorf("unexpected twilio config: %+v", cfg.Twilio)
	}
}

func TestLoad_TwilioPartialConfigRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEPLOYBOT_TWILIO_ACCOUNT_SID", "AC123")
	// AuthToken, From, To intentionally omitted.
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for partially configured Twilio")
	}
}

func TestLoad_MonitorInterval_Defaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MonitorInterval != 60*time.Second {
		t.Fatalf("MonitorInterval default = %v, want 60s", cfg.MonitorInterval)
	}
}

func TestLoad_MonitorInterval_Parses(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEPLOYBOT_MONITOR_INTERVAL", "2m")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MonitorInterval != 2*time.Minute {
		t.Fatalf("MonitorInterval = %v, want 2m", cfg.MonitorInterval)
	}
}

func TestLoad_MonitorInterval_BadValue(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DEPLOYBOT_MONITOR_INTERVAL", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid DEPLOYBOT_MONITOR_INTERVAL value")
	}
}
