package config

import (
	"bytes"
	"encoding/base64"
	"log"
	"strings"
	"testing"
	"time"

	"nori/internal/auth"
)

func validKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NORI_KEY", validKey())
	t.Setenv("NORI_SESSION_KEY", validKey())
	hash, err := auth.HashPassword("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NORI_ADMIN_HASH", hash)
}

func setNoriRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DEPLOYBOT_KEY", "")
	t.Setenv("DEPLOYBOT_SESSION_KEY", "")
	t.Setenv("DEPLOYBOT_ADMIN_HASH", "")
	t.Setenv("NORI_KEY", validKey())
	t.Setenv("NORI_SESSION_KEY", validKey())
	hash, err := auth.HashPassword("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NORI_ADMIN_HASH", hash)
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var warnings bytes.Buffer
	original := log.Writer()
	log.SetOutput(&warnings)
	t.Cleanup(func() { log.SetOutput(original) })
	return &warnings
}

func TestLoad_NoriEnvironmentTakesPrecedence(t *testing.T) {
	setNoriRequiredEnv(t)
	t.Setenv("NORI_TERMINAL_DIR", "/srv/nori")
	t.Setenv("DEPLOYBOT_TERMINAL_DIR", "/srv/legacy")
	warnings := captureWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TerminalDir != "/srv/nori" {
		t.Errorf("TerminalDir = %q, want /srv/nori", cfg.TerminalDir)
	}
	if strings.Contains(warnings.String(), "DEPLOYBOT_TERMINAL_DIR") {
		t.Errorf("legacy variable was not used but produced warning: %s", warnings.String())
	}
}

func TestLoad_LegacyEnvironmentWarns(t *testing.T) {
	setNoriRequiredEnv(t)
	t.Setenv("NORI_TERMINAL_DIR", "")
	t.Setenv("DEPLOYBOT_TERMINAL_DIR", "/srv/legacy")
	warnings := captureWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TerminalDir != "/srv/legacy" {
		t.Errorf("TerminalDir = %q, want /srv/legacy", cfg.TerminalDir)
	}
	if got := warnings.String(); !strings.Contains(got, "DEPLOYBOT_TERMINAL_DIR is deprecated; use NORI_TERMINAL_DIR") {
		t.Errorf("warning = %q, want legacy and replacement names", got)
	}
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
	t.Setenv("NORI_TERMINAL_DIR", "/srv/nori")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TerminalDir != "/srv/nori" {
		t.Errorf("TerminalDir = %q", cfg.TerminalDir)
	}
}

func TestLoad_MissingKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NORI_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing NORI_KEY")
	}
}

func TestLoad_BadKeyLength(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NORI_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
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
	t.Setenv("NORI_TWILIO_ACCOUNT_SID", "AC123")
	t.Setenv("NORI_TWILIO_AUTH_TOKEN", "tok")
	t.Setenv("NORI_TWILIO_FROM", "+15551234567")
	t.Setenv("NORI_TWILIO_TO", "+15559876543")
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
	t.Setenv("NORI_TWILIO_ACCOUNT_SID", "AC123")
	// AuthToken, From, To intentionally omitted.
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for partially configured Twilio")
	}
}

func TestLoad_TelegramDisabledByDefault(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Telegram.Enabled() {
		t.Fatal("Telegram must be disabled when env vars are absent")
	}
}

func TestLoad_TelegramEnabledWhenFullyConfigured(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NORI_TELEGRAM_BOT_TOKEN", "123:abc")
	t.Setenv("NORI_TELEGRAM_CHAT_ID", "-1001234567890")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Telegram.Enabled() {
		t.Fatal("Telegram must be enabled when both vars are set")
	}
	if cfg.Telegram.BotToken != "123:abc" || cfg.Telegram.ChatID != "-1001234567890" {
		t.Errorf("unexpected telegram config: %+v", cfg.Telegram)
	}
}

func TestLoad_LegacyTelegramEnvironmentWarns(t *testing.T) {
	setNoriRequiredEnv(t)
	t.Setenv("NORI_TELEGRAM_BOT_TOKEN", "")
	t.Setenv("NORI_TELEGRAM_CHAT_ID", "")
	t.Setenv("DEPLOYBOT_TELEGRAM_BOT_TOKEN", "123:legacy")
	t.Setenv("DEPLOYBOT_TELEGRAM_CHAT_ID", "-1001234567890")
	warnings := captureWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Telegram.Enabled() || cfg.Telegram.BotToken != "123:legacy" {
		t.Fatalf("legacy Telegram config was not loaded: %+v", cfg.Telegram)
	}
	for _, want := range []string{
		"DEPLOYBOT_TELEGRAM_BOT_TOKEN is deprecated; use NORI_TELEGRAM_BOT_TOKEN",
		"DEPLOYBOT_TELEGRAM_CHAT_ID is deprecated; use NORI_TELEGRAM_CHAT_ID",
	} {
		if !strings.Contains(warnings.String(), want) {
			t.Errorf("warning = %q, want %q", warnings.String(), want)
		}
	}
}

func TestLoad_TelegramPartialConfigRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NORI_TELEGRAM_BOT_TOKEN", "123:abc")
	// ChatID intentionally omitted.
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for partially configured Telegram")
	}
}

func TestLoad_TelegramAndTwilioCoexist(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NORI_TWILIO_ACCOUNT_SID", "AC123")
	t.Setenv("NORI_TWILIO_AUTH_TOKEN", "tok")
	t.Setenv("NORI_TWILIO_FROM", "+15551234567")
	t.Setenv("NORI_TWILIO_TO", "+15559876543")
	t.Setenv("NORI_TELEGRAM_BOT_TOKEN", "123:abc")
	t.Setenv("NORI_TELEGRAM_CHAT_ID", "-1001234567890")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Twilio.Enabled() || !cfg.Telegram.Enabled() {
		t.Fatalf("both channels must be enabled: twilio=%+v telegram=%+v", cfg.Twilio, cfg.Telegram)
	}
}

func mergeMaps(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
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
	t.Setenv("NORI_MONITOR_INTERVAL", "2m")
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
	t.Setenv("NORI_MONITOR_INTERVAL", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid NORI_MONITOR_INTERVAL value")
	}
}

func TestLoad_IntervalsMustBePositive(t *testing.T) {
	for _, name := range []string{"NORI_POLL_INTERVAL", "NORI_MONITOR_INTERVAL"} {
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
		"NORI_TWILIO_ACCOUNT_SID": "AC123",
		"NORI_TWILIO_AUTH_TOKEN":  "token",
		"NORI_TWILIO_FROM":        "+15551234567",
		"NORI_TWILIO_TO":          "+15559876543",
	}
	validTelegram := map[string]string{
		"NORI_TELEGRAM_BOT_TOKEN": "123:abc",
		"NORI_TELEGRAM_CHAT_ID":   "-1001234567890",
	}
	tests := []struct {
		name    string
		values  map[string]string
		wantErr bool
	}{
		{
			name: "known values and unknown passthrough",
			values: map[string]string{
				"NORI_POLL_INTERVAL":    "15s",
				"NORI_MONITOR_INTERVAL": "2m",
				"VIRTUAL_HOST":          "nori.example.com",
			},
		},
		{name: "invalid poll duration", values: map[string]string{"NORI_POLL_INTERVAL": "soon"}, wantErr: true},
		{name: "zero monitor duration", values: map[string]string{"NORI_MONITOR_INTERVAL": "0s"}, wantErr: true},
		{name: "negative poll duration", values: map[string]string{"NORI_POLL_INTERVAL": "-1s"}, wantErr: true},
		{name: "Twilio disabled", values: map[string]string{}},
		{name: "Twilio complete", values: validTwilio},
		{name: "Twilio partial", values: map[string]string{"NORI_TWILIO_ACCOUNT_SID": "AC123"}, wantErr: true},
		{name: "Telegram disabled", values: map[string]string{}},
		{name: "Telegram complete", values: validTelegram},
		{name: "Telegram partial", values: map[string]string{"NORI_TELEGRAM_BOT_TOKEN": "tok"}, wantErr: true},
		{name: "Twilio and Telegram complete", values: mergeMaps(validTwilio, validTelegram)},
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
