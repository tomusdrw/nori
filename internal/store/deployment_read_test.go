package store

import (
	"context"
	"strings"
	"testing"
)

func TestDeploymentBoundedReads(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "nginx", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	d := &Deployment{ServiceID: svc.ID, Trigger: TriggerManual, Status: DeploySuccess, Log: strings.Repeat("large-output", 100000) + "tail"}
	if err := st.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, 4, 100} {
		got, err := st.GetDeploymentWithLogLimit(ctx, d.ID, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Log) != limit || got.Log != d.Log[len(d.Log)-limit:] {
			t.Fatalf("log not limited to %d bytes", limit)
		}
	}
	rows, err := st.ListDeploymentSummaries(ctx, svc.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].Log != "" || rows[0].ID != d.ID {
		t.Fatalf("summaries: %+v %v", rows, err)
	}
}
