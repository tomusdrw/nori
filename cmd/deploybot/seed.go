package main

import (
	"context"

	"deploybot/internal/store"
)

func seedDemo(ctx context.Context, st *store.Store) error {
	return st.CreateService(ctx, &store.Service{
		Name:         "demo",
		WatchedImage: "ghcr.io/library/hello-world:latest",
		Policy:       store.PolicyManual,
	})
}
