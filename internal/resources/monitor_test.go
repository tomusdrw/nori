package resources

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nori/internal/store"
)

func resourceTestStore(t *testing.T) (*store.Store, *store.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &store.Service{Name: "api", Policy: store.PolicyManual}
	if err := st.CreateService(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	return st, svc
}

func TestStorageAlertTransitionsAndBaselines(t *testing.T) {
	ctx := t.Context()
	st, svc := resourceTestStore(t)
	if err := st.SetResourceLimits(ctx, svc.ID, store.ResourceLimits{UsageBytes: 1100, GrowthBytes: 500}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour)
	sample := func(at time.Time, b float64) store.ResourceSnapshot {
		return store.ResourceSnapshot{At: at, Readings: []store.ResourceReading{
			{Key: "volume:data", Kind: "volume", State: "available", Services: []int64{svc.ID}, Values: map[string]float64{"bytes": b}},
			{Key: "pg:db", Kind: "postgres-database", State: "available", Services: []int64{svc.ID}, Values: map[string]float64{"logical": 10000}},
		}}
	}
	for _, snap := range []store.ResourceSnapshot{sample(at.Add(-24*time.Hour), 100), sample(at.Add(-12*time.Hour), 500)} {
		if err := st.SaveResourceSnapshot(ctx, "storage", snap); err != nil {
			t.Fatal(err)
		}
	}
	var messages []string
	send := func(_ context.Context, msg, path string) error {
		if path != "/services/api/resources" {
			t.Fatalf("path %q", path)
		}
		messages = append(messages, msg)
		return nil
	}
	m := NewMonitor(st, nil, send)
	run := func(snap store.ResourceSnapshot) {
		t.Helper()
		if err := m.checkAlerts(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	run(sample(at, 1200))
	// New monitor simulates a process restart with the same SQLite database.
	m = NewMonitor(st, nil, send)
	run(sample(at, 1200))
	if len(messages) != 2 || !strings.Contains(messages[0], "uses") || !strings.Contains(messages[1], "grew") {
		t.Fatalf("notifications %v", messages)
	}
	partial := sample(at, 600)
	partial.Readings = append(partial.Readings, store.ResourceReading{Key: "missing", Kind: "volume", State: "unavailable", Services: []int64{svc.ID}})
	run(partial)
	run(sample(at, 1200))
	if len(messages) != 2 {
		t.Fatalf("partial measurement rearmed an alert: %v", messages)
	}
	run(sample(at, 600))
	run(sample(at, 1200))
	if len(messages) != 4 {
		t.Fatalf("recovery did not rearm both alerts: %v", messages)
	}
}

func TestFailedAlertDeliveryIsRetried(t *testing.T) {
	st, svc := resourceTestStore(t)
	if err := st.SetResourceLimits(t.Context(), svc.ID, store.ResourceLimits{UsageBytes: 10}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := NewMonitor(st, nil, func(context.Context, string, string) error {
		calls++
		if calls == 1 {
			return errors.New("unreachable")
		}
		return nil
	})
	snap := store.ResourceSnapshot{At: time.Now(), Readings: []store.ResourceReading{{Key: "x", Kind: "volume", Services: []int64{svc.ID}, Values: map[string]float64{"bytes": 10}}}}
	if err := m.checkAlerts(t.Context(), snap); err == nil {
		t.Fatal("missing delivery failure")
	}
	for range 2 {
		if err := m.checkAlerts(t.Context(), snap); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("send attempts %d", calls)
	}
}

type collectFunc func(context.Context, string) ([]store.ResourceReading, error)

func (f collectFunc) Collect(ctx context.Context, stream string) ([]store.ResourceReading, error) {
	return f(ctx, stream)
}

func TestRuntimeCollectionContinuesWhileStorageWaits(t *testing.T) {
	st, _ := resourceTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	storageStarted := make(chan struct{})
	sampled := make(chan struct{})
	m := NewMonitor(st, collectFunc(func(ctx context.Context, stream string) ([]store.ResourceReading, error) {
		if stream == "storage" {
			close(storageStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		<-storageStarted
		close(sampled)
		return nil, nil
	}), nil)
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	select {
	case <-sampled:
	case <-time.After(5 * time.Second):
		t.Fatal("storage blocked runtime")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not stop")
	}
}

func TestRuntimeAlertSumsContainersAndKeepsLatchOnCounterResets(t *testing.T) {
	st, svc := resourceTestStore(t)
	ctx := t.Context()
	if err := st.SetResourceLimits(ctx, svc.ID, store.ResourceLimits{Runtime: map[string]float64{"cpu": 150}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := NewMonitor(st, nil, func(context.Context, string, string) error { calls++; return nil })
	run := func(first, second store.ResourceReading) {
		first.Services = []int64{svc.ID}
		second.Services = []int64{svc.ID}
		if err := m.checkRuntimeAlerts(ctx, store.ResourceSnapshot{Readings: []store.ResourceReading{first, second}}); err != nil {
			t.Fatal(err)
		}
	}
	hot := store.ResourceReading{State: "running", Values: map[string]float64{"cpu": 80}}
	fresh := store.ResourceReading{State: "running"}
	stopped := store.ResourceReading{State: "stopped"}
	run(hot, hot)
	run(hot, fresh)
	run(hot, hot)
	if calls != 1 {
		t.Fatalf("missing counter rearmed alarm: %d calls", calls)
	}
	run(hot, stopped)
	run(hot, hot)
	if calls != 2 {
		t.Fatalf("expected second threshold crossing, got %d", calls)
	}
}

func TestStorageAlertRearmsWithPersistentlyUnavailableLogs(t *testing.T) {
	st, svc := resourceTestStore(t)
	ctx := t.Context()
	if err := st.SetResourceLimits(ctx, svc.ID, store.ResourceLimits{UsageBytes: 100}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := NewMonitor(st, nil, func(context.Context, string, string) error { calls++; return nil })
	for _, size := range []float64{120, 80, 130} {
		snap := store.ResourceSnapshot{Readings: []store.ResourceReading{
			{Key: "logs:unmounted", Kind: "logs", State: "unavailable", Services: []int64{svc.ID}},
			{Key: "volume:data", Kind: "volume", State: "available", Services: []int64{svc.ID}, Values: map[string]float64{"bytes": size}},
		}}
		if err := m.checkAlerts(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected two threshold crossings, got %d", calls)
	}
}
