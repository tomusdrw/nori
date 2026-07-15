package store

import (
	"context"
	"testing"
)

func TestDeploymentCRUD(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	d := &Deployment{ServiceID: svc.ID, Trigger: TriggerManual, TargetDigest: "sha256:abc", Status: DeployRunning}
	if err := st.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.ID == 0 {
		t.Fatal("ID not set")
	}

	got, err := st.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != DeployRunning || got.TargetDigest != "sha256:abc" {
		t.Fatalf("unexpected: %+v", got)
	}

	list, err := st.ListDeployments(ctx, svc.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
}
