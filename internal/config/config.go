package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr        string
	DBPath            string
	EncryptionKey     []byte
	SessionKey        []byte
	AdminPasswordHash string
	DockerHost        string
	TerminalDir       string
	PollInterval      time.Duration
	MonitorInterval   time.Duration
	Twilio            TwilioConfig
}

// TwilioConfig holds optional Twilio SMS settings. When every field is empty,
// SMS notifications are disabled and the app behaves as before. Partial
// configuration is rejected so a typo does not silently disable alerts.
type TwilioConfig struct {
	AccountSID string
	AuthToken  string
	From       string
	To         string
}

// Enabled reports whether all required Twilio fields are present.
func (t TwilioConfig) Enabled() bool {
	return t.AccountSID != "" && t.AuthToken != "" && t.From != "" && t.To != ""
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:      getenv("DEPLOYBOT_LISTEN", ":8080"),
		DBPath:          getenv("DEPLOYBOT_DB", "deploybot.db"),
		DockerHost:      os.Getenv("DEPLOYBOT_DOCKER_HOST"),
		TerminalDir:     getenv("DEPLOYBOT_TERMINAL_DIR", "."),
		PollInterval:    60 * time.Second,
		MonitorInterval: 60 * time.Second,
	}
	keyB64 := os.Getenv("DEPLOYBOT_KEY")
	if keyB64 == "" {
		return Config{}, fmt.Errorf("DEPLOYBOT_KEY is required (base64-encoded 32 bytes)")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return Config{}, fmt.Errorf("DEPLOYBOT_KEY: %w", err)
	}
	if len(key) != 32 {
		return Config{}, fmt.Errorf("DEPLOYBOT_KEY must decode to 32 bytes, got %d", len(key))
	}
	c.EncryptionKey = key

	sessionB64 := os.Getenv("DEPLOYBOT_SESSION_KEY")
	if sessionB64 == "" {
		return Config{}, fmt.Errorf("DEPLOYBOT_SESSION_KEY is required (base64-encoded 32 bytes)")
	}
	sessionKey, err := base64.StdEncoding.DecodeString(sessionB64)
	if err != nil {
		return Config{}, fmt.Errorf("DEPLOYBOT_SESSION_KEY: %w", err)
	}
	if len(sessionKey) < 32 {
		return Config{}, fmt.Errorf("DEPLOYBOT_SESSION_KEY must decode to at least 32 bytes, got %d", len(sessionKey))
	}
	c.SessionKey = sessionKey

	c.AdminPasswordHash = os.Getenv("DEPLOYBOT_ADMIN_HASH")
	if c.AdminPasswordHash == "" {
		return Config{}, fmt.Errorf("DEPLOYBOT_ADMIN_HASH is required (bcrypt hash of admin password)")
	}

	if v := os.Getenv("DEPLOYBOT_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEPLOYBOT_POLL_INTERVAL: %w", err)
		}
		c.PollInterval = d
	}

	if v := os.Getenv("DEPLOYBOT_MONITOR_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEPLOYBOT_MONITOR_INTERVAL: %w", err)
		}
		c.MonitorInterval = d
	}

	tw, err := loadTwilio()
	if err != nil {
		return Config{}, err
	}
	c.Twilio = tw
	return c, nil
}

func loadTwilio() (TwilioConfig, error) {
	t := TwilioConfig{
		AccountSID: strings.TrimSpace(os.Getenv("DEPLOYBOT_TWILIO_ACCOUNT_SID")),
		AuthToken:  os.Getenv("DEPLOYBOT_TWILIO_AUTH_TOKEN"),
		From:       strings.TrimSpace(os.Getenv("DEPLOYBOT_TWILIO_FROM")),
		To:         strings.TrimSpace(os.Getenv("DEPLOYBOT_TWILIO_TO")),
	}
	// Treat "all empty" as "intentionally disabled". Any partial set is an
	// operator error: half-configured Twilio would silently never fire and
	// defeat the purpose of wiring it up.
	any := t.AccountSID != "" || t.AuthToken != "" || t.From != "" || t.To != ""
	if !any {
		return TwilioConfig{}, nil
	}
	var missing []string
	if t.AccountSID == "" {
		missing = append(missing, "DEPLOYBOT_TWILIO_ACCOUNT_SID")
	}
	if t.AuthToken == "" {
		missing = append(missing, "DEPLOYBOT_TWILIO_AUTH_TOKEN")
	}
	if t.From == "" {
		missing = append(missing, "DEPLOYBOT_TWILIO_FROM")
	}
	if t.To == "" {
		missing = append(missing, "DEPLOYBOT_TWILIO_TO")
	}
	if len(missing) > 0 {
		return TwilioConfig{}, fmt.Errorf(
			"DEPLOYBOT_TWILIO_* partially configured: missing %s", strings.Join(missing, ", "))
	}
	return t, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
