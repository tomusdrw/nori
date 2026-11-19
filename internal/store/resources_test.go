package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestResourceHistoryRetentionAndRetry(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	shared := ResourceReading{Key: "vol", Kind: "volume", Services: []int64{1, 2}, Values: map[string]float64{"bytes": 0}}
	missing := ResourceReading{Key: "old", State: "unavailable"}
	save := func(stream string, at time.Time, readings ...ResourceReading) {
		t.Helper()
		if err := s.SaveResourceSnapshot(ctx, stream, ResourceSnapshot{At: at, Readings: readings}); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-2 * 24 * time.Hour)
	earlier := old.Add(time.Minute)
	last := old.Add(50 * time.Minute)
	save("runtime", old, missing)
	save("storage", now.Add(-31*24*time.Hour), shared)
	save("storage", earlier, shared)
	save("storage", last, shared, missing)
	save("storage", now.Add(-time.Hour), missing)
	save("storage", now.Add(-time.Minute), shared)
	save("storage", now, shared)
	// A delayed sample must not displace a later hourly representative or resurrect expired detail.
	save("storage", earlier, missing)
	save("storage", now, shared, missing)
	history, err := s.resourceSnapshots(ctx, "SELECT at, readings FROM resource_sample WHERE stream=? ORDER BY at", "storage")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || !history[0].At.Equal(last) || !reflect.DeepEqual(history[0].Readings, []ResourceReading{shared, missing}) {
		t.Fatalf("retained history: %+v", history)
	}
	if !reflect.DeepEqual(history[3].Readings, []ResourceReading{shared, missing}) {
		t.Fatalf("retry: %+v", history[3])
	}
	empty, err := s.LatestResourceSnapshot(ctx, "storage", earlier)
	if err != nil || !empty.At.IsZero() {
		t.Fatalf("empty snapshot = %+v, %v", empty, err)
	}
	latest, err := s.LatestResourceSnapshot(ctx, "runtime", time.Time{})
	if err != nil || !latest.At.Equal(old) {
		t.Fatalf("other stream = %+v, %v", latest, err)
	}
	exact, err := s.LatestResourceSnapshot(ctx, "storage", last)
	if err != nil || !exact.At.Equal(last) {
		t.Fatalf("exact boundary = %+v, %v", exact, err)
	}
}

func TestResourceChartReturnsRealSamplesIncludingGaps(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for minute := range 6 {
		owner := int64(1)
		if minute == 3 {
			owner = 2
		}
		reading := ResourceReading{Services: []int64{owner}, Values: map[string]float64{"bytes": float64(minute)}}
		err := s.SaveResourceSnapshot(ctx, "storage", ResourceSnapshot{At: start.Add(time.Duration(minute) * time.Minute), Readings: []ResourceReading{reading}})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ResourceChartHistory(ctx, "storage", start, start.Add(6*time.Minute-time.Nanosecond), 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[0].At.Equal(start.Add(time.Minute)) || got[0].Readings[0].Values["bytes"] != 1 || !got[1].At.Equal(start.Add(3*time.Minute)) || len(got[1].Readings) != 0 || got[2].Readings[0].Values["bytes"] != 5 {
		t.Fatalf("chart = %+v", got)
	}
}

func TestResourceLimitsAndAlertState(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	svc := &Service{Name: "service", Policy: PolicyManual}
	if err := s.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	limits, err := s.ResourceLimits(ctx, svc.ID)
	if err != nil || !reflect.DeepEqual(limits, ResourceLimits{}) {
		t.Fatalf("defaults = %+v, %v", limits, err)
	}
	limits = ResourceLimits{UsageBytes: 100, GrowthBytes: 20, PostgresDetails: true}
	if err := s.SetResourceLimits(ctx, svc.ID, limits); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"usage", "growth"} {
		if err := s.SetResourceAlertIfCurrent(ctx, svc.ID, kind, true, limits, ""); err != nil {
			t.Fatal(err)
		}
	}
	limits.UsageBytes++
	if err := s.SetResourceLimits(ctx, svc.ID, limits); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResourceLimits(ctx, svc.ID)
	if err != nil || !reflect.DeepEqual(got, limits) {
		t.Fatalf("limits = %+v, %v", got, err)
	}
	for kind, want := range map[string]bool{"usage": false, "growth": true} {
		_, active, err := s.ResourceAlertCoverage(ctx, svc.ID, kind)
		if err != nil || active != want {
			t.Fatalf("active %s = %v, %v", kind, active, err)
		}
	}
	if err := s.DeleteService(ctx, svc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResourceLimits(ctx, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted service: %v", err)
	}
}

func TestRuntimeThresholdChangesRearmOnlyChangedMetrics(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	svc := &Service{Name: "worker", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	limits := ResourceLimits{Runtime: map[string]float64{"cpu": 25, "pids": 10}}
	if err := st.SetResourceLimits(ctx, svc.ID, limits); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"runtime:cpu", "runtime:pids"} {
		if err := st.SetResourceAlertIfCurrent(ctx, svc.ID, kind, true, limits, ""); err != nil {
			t.Fatal(err)
		}
	}
	limits.Runtime["memory"] = 1024
	limits.Runtime["cpu"] = 50
	if err := st.SetResourceLimits(ctx, svc.ID, limits); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]bool{"runtime:cpu": false, "runtime:pids": true, "runtime:memory": false} {
		_, got, err := st.ResourceAlertCoverage(ctx, svc.ID, kind)
		if err != nil || got != want {
			t.Fatalf("%s = %t, %v", kind, got, err)
		}
	}
	active, err := st.ActiveResourceLimits(ctx)
	if err != nil || !reflect.DeepEqual(active, map[int64]ResourceLimits{svc.ID: limits}) {
		t.Fatalf("active limits = %+v, %v", active, err)
	}
	limits.Runtime["unknown"] = 1
	if err := st.SetResourceLimits(ctx, svc.ID, limits); err == nil {
		t.Fatal("accepted unknown metric")
	}
	if err := st.SetResourceLimits(ctx, svc.ID, ResourceLimits{}); err != nil {
		t.Fatal(err)
	}
	active, err = st.ActiveResourceLimits(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("disabled limits = %+v, %v", active, err)
	}
}

func TestResourceHistoryAndAlertStateSurviveReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "resources.db")
	key := make([]byte, 32)
	st, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Name: "app", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	limits := ResourceLimits{UsageBytes: 1024, PostgresDetails: true}
	if err := st.SetResourceLimits(ctx, svc.ID, limits); err != nil {
		t.Fatal(err)
	}
	if err := st.SetResourceAlertIfCurrent(ctx, svc.ID, "usage", true, limits, ""); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	inventory := ResourceReading{Key: "volumes", Kind: "inventory", State: "unavailable"}
	if err := st.SaveResourceSnapshot(ctx, "storage", ResourceSnapshot{At: at, Readings: []ResourceReading{inventory}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.ResourceLimits(ctx, svc.ID)
	if err != nil || !reflect.DeepEqual(got, limits) {
		t.Fatalf("limits after restart = %+v, %v", got, err)
	}
	_, active, err := st.ResourceAlertCoverage(ctx, svc.ID, "usage")
	if err != nil || !active {
		t.Fatalf("latch after restart = %t, %v", active, err)
	}
	history, err := st.ResourceChartHistory(ctx, "storage", at, at, 1, svc.ID)
	if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0].Readings, []ResourceReading{inventory}) {
		t.Fatalf("missing inventory in service history after restart: %+v, %v", history, err)
	}
}

func TestResourceDirectories(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	svc := &Service{Name: "site", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	valid := ResourceLimits{Directories: []string{"/media/data", "/srv/files", "/device"}}
	if err := st.SetResourceLimits(ctx, svc.ID, valid); err != nil {
		t.Fatal(err)
	}
	for _, dirs := range [][]string{{"/"}, {"/proc"}, {"/sys/firmware"}, {"/dev/null"}, {"relative"}, {"/data/../srv"}, {"/srv", "/srv"}, make([]string, 33)} {
		if err := st.SetResourceLimits(ctx, svc.ID, ResourceLimits{Directories: dirs}); err == nil {
			t.Fatalf("accepted invalid dirs %q", dirs)
		}
	}
	got, err := st.ActiveResourceLimits(ctx)
	if err != nil || !reflect.DeepEqual(got[svc.ID], valid) {
		t.Fatalf("saved dirs = %+v, %v", got, err)
	}
}

func TestResourceDeploymentMarkersFilterChartWindow(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	svc := &Service{Name: "blog", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, offset := range []time.Duration{-time.Hour, 0, time.Hour} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO deployment (service_id,trigger,status,started_at,log) VALUES(?, 'auto','success',?,'large log')`, svc.ID, now.Add(offset).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListResourceDeployments(ctx, svc.ID, now.Add(-time.Hour+time.Nanosecond), now)
	if err != nil || len(got) != 1 || !got[0].StartedAt.Equal(now) || got[0].Log != "" {
		t.Fatalf("markers = %+v, %v", got, err)
	}
}

func TestResourceAlertConcurrentSettingsChange(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	svc := &Service{Name: "app", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	before := ResourceLimits{UsageBytes: 12, GrowthBytes: 3, Directories: []string{"/a", "/b"}, Runtime: map[string]float64{"pids": 10}}
	if err := st.SetResourceLimits(ctx, svc.ID, before); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"usage", "growth", "runtime:pids"} {
		if err := st.SetResourceAlertIfCurrent(ctx, svc.ID, kind, true, before, "missing-logs"); err != nil {
			t.Fatal(err)
		}
	}
	coverage, active, err := st.ResourceAlertCoverage(ctx, svc.ID, "usage")
	if err != nil || !active || coverage != "missing-logs" {
		t.Fatalf("coverage after delivery = %q, %t, %v", coverage, active, err)
	}
	reordered := before
	reordered.Directories = []string{"/b", "/a"}
	if err := st.SetResourceLimits(ctx, svc.ID, reordered); err != nil {
		t.Fatal(err)
	}
	if _, active, err := st.ResourceAlertCoverage(ctx, svc.ID, "usage"); err != nil || !active {
		t.Fatalf("reordering directories cleared usage alert: %v", err)
	}
	after := before
	after.Directories = []string{"/a"}
	after.Runtime = map[string]float64{"pids": 20}
	if err := st.SetResourceLimits(ctx, svc.ID, after); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"usage", "growth", "runtime:pids"} {
		if err := st.SetResourceAlertIfCurrent(ctx, svc.ID, kind, true, before, "missing-logs"); err != nil {
			t.Fatal(err)
		}
		if _, active, err := st.ResourceAlertCoverage(ctx, svc.ID, kind); err != nil || active {
			t.Fatalf("stale %s delivery latched: %v", kind, err)
		}
	}
}
