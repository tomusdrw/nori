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
	DefaultBotName = "Nori"
	MaxBotNameLen  = 64

	// SettingNotifyMode stores the legacy in-app notification-mode toggle
	// (always / auto-only / never). It is superseded by SettingNotifyRouting
	// and only read as a fallback while no routing table has been saved. The
	// store treats both values as opaque strings so the lower layer has no
	// dependency on notification logic.
	SettingNotifyMode = "notify_mode"

	// SettingNotifyRouting stores the per-channel event routing table written
	// by the settings page. Its schema lives in internal/notify
	// (notify.Routing); the store treats it as an opaque string.
	SettingNotifyRouting = "notify_routing"
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

// NotifyMode returns the stored legacy notification-mode string (e.g.
// "always", "auto-only", "never"), or "" when unset. Validation of the value
// belongs to the caller.
func (s *Store) NotifyMode(ctx context.Context) string {
	value, err := s.GetSetting(ctx, SettingNotifyMode)
	if err != nil {
		return ""
	}
	return value
}

// NotifyRoutingRaw returns the stored notification-routing JSON, or "" when
// unset. Parsing belongs to the caller (see internal/notify.ParseRouting).
func (s *Store) NotifyRoutingRaw(ctx context.Context) string {
	value, err := s.GetSetting(ctx, SettingNotifyRouting)
	if err != nil {
		return ""
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
