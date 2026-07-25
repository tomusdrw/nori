package notify

import (
	"fmt"
	"strings"
)

// Mode controls when service-down alerts are sent.
type Mode string

const (
	// ModeAlways sends alerts for every failure (the default). Auto-triggered
	// deploys are still deduped by the executor's per-digest cooldown; manual
	// deploys are not deduped.
	ModeAlways Mode = "always"
	// ModeAutoOnly suppresses alerts for manually triggered deploys. Auto
	// and scheduled failures still alert.
	ModeAutoOnly Mode = "auto-only"
	// ModeNever suppresses all alerts regardless of trigger.
	ModeNever Mode = "never"
)

// DefaultMode is used when the setting is unset or empty. It preserves the
// pre-setting behavior so existing installations keep alerting.
const DefaultMode Mode = ModeAlways

// SettingKey is the storage key used for the in-app notification-mode toggle.
// Defined here so store and web layers share a single source of truth.
const SettingKey = "notify_mode"

// NormalizeMode trims, lowercases, and validates a mode string. An empty
// input resolves to DefaultMode so a missing setting behaves like "always".
func NormalizeMode(s string) (Mode, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return DefaultMode, nil
	}
	switch Mode(s) {
	case ModeAlways, ModeAutoOnly, ModeNever:
		return Mode(s), nil
	}
	return "", fmt.Errorf("invalid notification mode %q (want always, auto-only, or never)", s)
}

// ShouldSend reports whether an event with the given trigger should be
// forwarded to the inner notifier under the given mode. trigger follows the
// store.Trigger* string values ("manual", "auto", "scheduled", "monitor").
// "monitor" is an automatic signal, so it sends under auto-only exactly like
// "auto" and "scheduled"; TestShouldSend locks this in.
func ShouldSend(mode Mode, trigger string) bool {
	switch mode {
	case ModeNever:
		return false
	case ModeAutoOnly:
		return trigger != "manual"
	default:
		return true
	}
}
