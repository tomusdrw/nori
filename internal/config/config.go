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
	Telegram          TelegramConfig
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

// TelegramConfig holds optional Telegram notification settings. When both
// fields are empty, Telegram notifications are disabled. Partial
// configuration is rejected so a typo does not silently disable alerts.
type TelegramConfig struct {
	BotToken string
	ChatID   string
}

// Enabled reports whether all required Telegram fields are present.
func (t TelegramConfig) Enabled() bool {
	return t.BotToken != "" && t.ChatID != ""
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
		c.PollInterval, err = parsePositiveDuration("DEPLOYBOT_POLL_INTERVAL", v)
		if err != nil {
			return Config{}, err
		}
	}

	if v := os.Getenv("DEPLOYBOT_MONITOR_INTERVAL"); v != "" {
		c.MonitorInterval, err = parsePositiveDuration("DEPLOYBOT_MONITOR_INTERVAL", v)
		if err != nil {
			return Config{}, err
		}
	}

	tw, err := loadTwilio(os.Getenv)
	if err != nil {
		return Config{}, err
	}
	c.Twilio = tw

	tg, err := loadTelegram(os.Getenv)
	if err != nil {
		return Config{}, err
	}
	c.Telegram = tg
	return c, nil
}

// ValidateEnvironment validates the known runtime settings that an operator
// may provide through an environment editor. Unknown keys remain valid so
// Docker, reverse-proxy, and future application settings can pass through.
func ValidateEnvironment(values map[string]string) error {
	for _, name := range []string{"DEPLOYBOT_POLL_INTERVAL", "DEPLOYBOT_MONITOR_INTERVAL"} {
		if value := values[name]; value != "" {
			if _, err := parsePositiveDuration(name, value); err != nil {
				return err
			}
		}
	}
	_, err := loadTwilio(func(name string) string {
		return values[name]
	})
	if err != nil {
		return err
	}
	_, err = loadTelegram(func(name string) string {
		return values[name]
	})
	return err
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return duration, nil
}

func loadTwilio(get func(string) string) (TwilioConfig, error) {
	t := TwilioConfig{
		AccountSID: strings.TrimSpace(get("DEPLOYBOT_TWILIO_ACCOUNT_SID")),
		AuthToken:  get("DEPLOYBOT_TWILIO_AUTH_TOKEN"),
		From:       strings.TrimSpace(get("DEPLOYBOT_TWILIO_FROM")),
		To:         strings.TrimSpace(get("DEPLOYBOT_TWILIO_TO")),
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

func loadTelegram(get func(string) string) (TelegramConfig, error) {
	t := TelegramConfig{
		BotToken: get("DEPLOYBOT_TELEGRAM_BOT_TOKEN"),
		ChatID:   strings.TrimSpace(get("DEPLOYBOT_TELEGRAM_CHAT_ID")),
	}
	// Treat "all empty" as "intentionally disabled", mirroring Twilio: a
	// half-configured channel would silently never fire and defeat the
	// purpose of wiring it up.
	if t.BotToken == "" && t.ChatID == "" {
		return TelegramConfig{}, nil
	}
	var missing []string
	if t.BotToken == "" {
		missing = append(missing, "DEPLOYBOT_TELEGRAM_BOT_TOKEN")
	}
	if t.ChatID == "" {
		missing = append(missing, "DEPLOYBOT_TELEGRAM_CHAT_ID")
	}
	if len(missing) > 0 {
		return TelegramConfig{}, fmt.Errorf(
			"DEPLOYBOT_TELEGRAM_* partially configured: missing %s", strings.Join(missing, ", "))
	}
	return t, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
