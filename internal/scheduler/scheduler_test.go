package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"deploybot/internal/store"
)

func TestScheduledCallbackLookupErrors(t *testing.T) {
	for _, failure := range []string{"closed database", "deleted service", "cancelled context"} {
		t.Run(failure, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "scheduler.db"), make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc := &store.Service{Name: "lookup-test", WatchedImage: "nginx:latest", Policy: store.PolicyScheduled, CronExpr: "0 3 * * *"}
			if err := st.CreateService(ctx, svc); err != nil {
				t.Fatal(err)
			}
			s := New(st, nil)
			if err := s.reload(ctx); err != nil {
				t.Fatal(err)
			}
			job := s.cron.Entry(s.entries[svc.ID].id).Job
			switch failure {
			case "closed database":
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
			case "deleted service":
				if err := st.DeleteService(ctx, svc.ID); err != nil {
					t.Fatal(err)
				}
			case "cancelled context":
				cancel()
			}
			var output bytes.Buffer
			previousWriter := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previousWriter)
			// Run the queued callback against the changed database/context. A nil
			// executor also ensures none of these lookup failures can deploy.
			job.Run()
			if failure == "closed database" {
				for _, want := range []string{`load service "lookup-test"`, fmt.Sprintf("id %d", svc.ID), "database is closed"} {
					if !strings.Contains(output.String(), want) {
						t.Errorf("log %q does not contain %q", output.String(), want)
					}
				}
			} else if output.Len() != 0 {
				t.Errorf("expected lookup failure should be quiet: %s", output.String())
			}
		})
	}
}

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
