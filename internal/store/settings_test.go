package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBotNameDefault(t *testing.T) {
	st := testStore(t)
	if got := st.BotName(context.Background()); got != DefaultBotName {
		t.Fatalf("BotName = %q, want %q", got, DefaultBotName)
	}
}

func TestBotNameRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.SetSetting(ctx, SettingBotName, "Staging Bot"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := st.BotName(ctx); got != "Staging Bot" {
		t.Fatalf("BotName = %q, want Staging Bot", got)
	}
}

func TestNormalizeBotName(t *testing.T) {
	got, err := NormalizeBotName("  Prod  ")
	if err != nil || got != "Prod" {
		t.Fatalf("NormalizeBotName = %q, %v", got, err)
	}
	if _, err := NormalizeBotName("   "); !errors.Is(err, ErrBotNameEmpty) {
		t.Fatalf("empty: got %v", err)
	}
	if _, err := NormalizeBotName(strings.Repeat("a", MaxBotNameLen+1)); !errors.Is(err, ErrBotNameTooLong) {
		t.Fatalf("too long: got %v", err)
	}
}
