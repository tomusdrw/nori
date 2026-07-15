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

func TestReconcileSelfDeploymentsUsesRunningDigest(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	self, err := st.EnsureSelfService(ctx, "ghcr.io/acme/deploybot:latest")
	if err != nil {
		t.Fatal(err)
	}
	matching := &Deployment{ServiceID: self.ID, Trigger: TriggerManual, TargetDigest: "sha256:match", Status: DeployRunning}
	mismatch := &Deployment{ServiceID: self.ID, Trigger: TriggerManual, TargetDigest: "sha256:old", Status: DeployRunning}
	for _, d := range []*Deployment{matching, mismatch} {
		if err := st.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReconcileSelfDeployments(ctx, "sha256:match"); err != nil {
		t.Fatal(err)
	}
	gotMatch, _ := st.GetDeployment(ctx, matching.ID)
	gotMismatch, _ := st.GetDeployment(ctx, mismatch.ID)
	if gotMatch.Status != DeploySuccess || gotMatch.FinishedAt == nil {
		t.Fatalf("matching deployment = %+v", gotMatch)
	}
	if gotMismatch.Status != DeployFailed || gotMismatch.FinishedAt == nil {
		t.Fatalf("mismatched deployment = %+v", gotMismatch)
	}
}
