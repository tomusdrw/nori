package config

import (
	"encoding/base64"
	"fmt"
	"os"
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
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:   getenv("DEPLOYBOT_LISTEN", ":8080"),
		DBPath:       getenv("DEPLOYBOT_DB", "deploybot.db"),
		DockerHost:   os.Getenv("DEPLOYBOT_DOCKER_HOST"),
		TerminalDir:  getenv("DEPLOYBOT_TERMINAL_DIR", "."),
		PollInterval: 60 * time.Second,
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
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
