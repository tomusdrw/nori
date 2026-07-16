package web

import (
	"context"

	"deploybot/internal/store"
)

type botNameCtxKey struct{}

func withBotName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, botNameCtxKey{}, name)
}

// BotName returns the instance display name from request context.
func BotName(ctx context.Context) string {
	if v, ok := ctx.Value(botNameCtxKey{}).(string); ok && v != "" {
		return v
	}
	return store.DefaultBotName
}
