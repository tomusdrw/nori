package scheduler

import (
	"context"
	"path/filepath"
	"testing"

	"deploybot/internal/store"
)

func TestReloadLiveServices(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "scheduler.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	s := New(st, nil)
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	svc := &store.Service{Name: "live", WatchedImage: "nginx:latest", Policy: store.PolicyScheduled, CronExpr: "0 3 * * *"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	first := s.entries[svc.ID]
	if first.id == 0 || len(s.cron.Entries()) != 1 {
		t.Fatal("new schedule not installed")
	}
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if s.entries[svc.ID].id != first.id {
		t.Fatal("unchanged job was reset")
	}
	stale := s.cron.Entry(first.id).Job
	svc.CronExpr = "0 4 * * *"
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	// A callback already queued under the old configuration must not deploy.
	stale.Run()
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if s.entries[svc.ID].id == first.id || len(s.cron.Entries()) != 1 {
		t.Fatal("changed cron not replaced")
	}
	svc.Policy = store.PolicyManual
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.entries) != 0 || len(s.cron.Entries()) != 0 {
		t.Fatal("manual service still scheduled")
	}
	svc.Policy = store.PolicyScheduled
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteService(ctx, svc.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.entries) != 0 {
		t.Fatal("deleted service still scheduled")
	}
	invalid := &store.Service{Name: "legacy-invalid", WatchedImage: "nginx", Policy: store.PolicyScheduled, CronExpr: "invalid"}
	if err := st.CreateService(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if entry, ok := s.entries[invalid.ID]; !ok || entry.id != 0 || entry.expr != "invalid" {
		t.Fatal("invalid schedule not remembered")
	}
	invalid.CronExpr = "0 5 * * *"
	if err := st.UpdateService(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(ctx); err != nil {
		t.Fatal(err)
	}
	if s.entries[invalid.ID].id == 0 {
		t.Fatal("corrected schedule did not activate")
	}
}
