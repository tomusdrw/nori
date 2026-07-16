package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	SettingBotName = "bot_name"
	DefaultBotName = "Deploy Bot"
	MaxBotNameLen  = 64
)

var (
	ErrBotNameEmpty   = errors.New("bot name is required")
	ErrBotNameTooLong = errors.New("bot name must be at most 64 characters")
)

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM setting WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return value, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO setting (key, value) VALUES (?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// BotName returns the configured display name, or DefaultBotName when unset.
func (s *Store) BotName(ctx context.Context) string {
	value, err := s.GetSetting(ctx, SettingBotName)
	if err != nil {
		return DefaultBotName
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultBotName
	}
	return value
}

// NormalizeBotName trims and validates a display name for storage.
func NormalizeBotName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrBotNameEmpty
	}
	if utf8.RuneCountInString(name) > MaxBotNameLen {
		return "", ErrBotNameTooLong
	}
	return name, nil
}
