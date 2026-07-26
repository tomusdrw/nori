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

func TestLoad_IntervalsMustBePositive(t *testing.T) {
	for _, name := range []string{"DEPLOYBOT_POLL_INTERVAL", "DEPLOYBOT_MONITOR_INTERVAL"} {
		for _, value := range []string{"0s", "-1s"} {
			t.Run(name+"="+value, func(t *testing.T) {
				setRequiredEnv(t)
				t.Setenv(name, value)
				if _, err := Load(); err == nil {
					t.Fatalf("expected error for %s=%s", name, value)
				}
			})
		}
	}
}

func TestValidateEnvironment(t *testing.T) {
	validTwilio := map[string]string{
		"DEPLOYBOT_TWILIO_ACCOUNT_SID": "AC123",
		"DEPLOYBOT_TWILIO_AUTH_TOKEN":  "token",
		"DEPLOYBOT_TWILIO_FROM":        "+15551234567",
		"DEPLOYBOT_TWILIO_TO":          "+15559876543",
	}
	tests := []struct {
		name    string
		values  map[string]string
		wantErr bool
	}{
		{
			name: "known values and unknown passthrough",
			values: map[string]string{
				"DEPLOYBOT_POLL_INTERVAL":    "15s",
				"DEPLOYBOT_MONITOR_INTERVAL": "2m",
				"VIRTUAL_HOST":               "nori.example.com",
			},
		},
		{name: "invalid poll duration", values: map[string]string{"DEPLOYBOT_POLL_INTERVAL": "soon"}, wantErr: true},
		{name: "zero monitor duration", values: map[string]string{"DEPLOYBOT_MONITOR_INTERVAL": "0s"}, wantErr: true},
		{name: "negative poll duration", values: map[string]string{"DEPLOYBOT_POLL_INTERVAL": "-1s"}, wantErr: true},
		{name: "Twilio disabled", values: map[string]string{}},
		{name: "Twilio complete", values: validTwilio},
		{name: "Twilio partial", values: map[string]string{"DEPLOYBOT_TWILIO_ACCOUNT_SID": "AC123"}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateEnvironment(test.values)
			if test.wantErr && err == nil {
				t.Fatal("ValidateEnvironment error = nil, want error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("ValidateEnvironment error = %v", err)
			}
		})
	}
}
