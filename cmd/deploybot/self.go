package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"deploybot/internal/docker"
	"deploybot/internal/store"
)

// initializeSelfService only runs in launcher-managed installations. It seeds
// the managed self-service before polling begins, then resolves any handoff
// left running by the instance that was just replaced.
func initializeSelfService(ctx context.Context, st *store.Store, dk docker.Client) error {
	image := os.Getenv("DEPLOYBOT_SELF_IMAGE")
	configVolume := os.Getenv("DEPLOYBOT_CONFIG_VOLUME")
	containerName := os.Getenv("DEPLOYBOT_SELF_CONTAINER")
	if image == "" && configVolume == "" && containerName == "" {
		return nil
	}
	if image == "" || configVolume == "" || containerName == "" {
		return errors.New("incomplete launcher identity environment")
	}
	svc, err := st.EnsureSelfService(ctx, image)
	if err != nil {
		return err
	}
	containers, err := dk.ListByService(ctx, svc.Name)
	if err != nil {
		return fmt.Errorf("find self container: %w", err)
	}
	return st.ReconcileSelfDeployments(ctx, docker.DigestForImage(containers, image))
}
