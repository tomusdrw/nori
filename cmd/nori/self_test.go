package main

import (
	"context"
	"path/filepath"
	"testing"

	"nori/internal/docker"
	"nori/internal/store"
)

func TestInitializeSelfServiceSeedsAndReconciles(t *testing.T) {
	t.Setenv("NORI_SELF_IMAGE", "ghcr.io/acme/nori:latest")
	t.Setenv("NORI_CONFIG_VOLUME", "nori-config")
	t.Setenv("NORI_SELF_CONTAINER", "nori")
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := initializeSelfService(ctx, st, &docker.Fake{Containers: map[string][]docker.Container{}}); err != nil {
		t.Fatal(err)
	}
	self, err := st.GetSelfService(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Deployment{ServiceID: self.ID, Trigger: store.TriggerManual, TargetDigest: "sha256:new", Status: store.DeployRunning}
	if err := st.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	dk := &docker.Fake{Containers: map[string][]docker.Container{
		store.SelfServiceName: {{Name: "nori", Image: "ghcr.io/acme/nori:latest", Digest: "sha256:new", State: "running"}},
	}}
	if err := initializeSelfService(ctx, st, dk); err != nil {
		t.Fatal(err)
	}
	resolved, err := st.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != store.DeploySuccess || resolved.FinishedAt == nil {
		t.Fatalf("deployment was not reconciled: %+v", resolved)
	}
}
