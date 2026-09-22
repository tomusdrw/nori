package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nori/internal/docker"
	"nori/internal/notify"
	"nori/internal/store"
)

type capturedEvent struct {
	Kind string
	E    notify.Event
}

type capturingNotifier struct {
	events []capturedEvent
}

func (c *capturingNotifier) NotifyServiceDown(_ context.Context, evt notify.Event) error {
	c.events = append(c.events, capturedEvent{Kind: "down", E: evt})
	return nil
}

func (c *capturingNotifier) NotifyServiceRecovered(_ context.Context, evt notify.Event) error {
	c.events = append(c.events, capturedEvent{Kind: "recovered", E: evt})
	return nil
}

func (c *capturingNotifier) NotifyDeploySuccess(_ context.Context, evt notify.Event) error {
	c.events = append(c.events, capturedEvent{Kind: "success", E: evt})
	return nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMonitor_SustainedUp_NoAlerts(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running", Health: "", Digest: "sha256:abc"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 100*time.Millisecond)
	// First tick: up; second tick: still up
	m.Tick(ctx)
	// flip to up again (still running)
	m.Tick(ctx)
	if len(n.events) != 0 {
		t.Fatalf("expected no alerts, got %d: %v", len(n.events), n.events)
	}
}

// Health URL probing tests
func TestMonitor_HealthURL_DownOnSecondTick(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc_health", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok", HealthURL: ""}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	// health_url server that returns 503
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("service unhealthy"))
	}))
	defer server.Close()
	// attach health URL to service via store update
	svc.HealthURL = server.URL
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running", Digest: "sha256:abc"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	// Tick 1: up with container, but health down => probeUp false
	m.Tick(ctx)
	// Tick 2: second consecutive tick, should alert
	m.Tick(ctx)
	if len(n.events) != 1 {
		t.Fatalf("expected 1 down event due to health URL, got %d: %+v", len(n.events), n.events)
	}
	if n.events[0].Kind != "down" || n.events[0].E.Trigger != store.TriggerMonitor {
		t.Fatalf("expected down event with monitor trigger: %+v", n.events[0])
	}
	if !strings.Contains(n.events[0].E.Reason, "status 503") {
		t.Fatalf("down event reason must name the failing status: %+v", n.events[0].E.Reason)
	}
}

func TestMonitor_HealthURL_RecoveryAfterGood(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc_health2", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	// server: first two requests return 503 to trigger a down alert, then 200 to simulate recovery
	var state int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state < 2 {
			w.WriteHeader(503)
			w.Write([]byte("service unhealthy"))
		} else {
			w.WriteHeader(200)
			w.Write([]byte("ok"))
		}
		state++
	}))
	defer server.Close()
	svc.HealthURL = server.URL
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running", Digest: "sha256:abc"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	// Tick 1: health down
	m.Tick(ctx)
	// Tick 2: down again (should trigger down alert)
	m.Tick(ctx)
	// Tick 3: health healthy again -> recovery
	m.Tick(ctx)
	if len(n.events) != 2 {
		t.Fatalf("expected down then recovery after health URL recovered, got events: %v", n.events)
	}
	if n.events[1].Kind != "recovered" || n.events[1].E.Trigger != store.TriggerMonitor {
		t.Fatalf("second event must be recovered with monitor trigger: %+v", n.events[1])
	}
}

func TestMonitor_HealthURL_OKNoAlerts(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	svc := &store.Service{Name: "svc_health3", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok", HealthURL: server.URL}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	m.Tick(ctx)
	m.Tick(ctx)
	if len(n.events) != 0 {
		t.Fatalf("expected no alerts for healthy service, got %+v", n.events)
	}
}

func TestMonitor_HealthURL_UnreachableCountsDown(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc_health4", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok", HealthURL: "http://127.0.0.1:1"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	m.Tick(ctx)
	m.Tick(ctx)
	if len(n.events) != 1 || n.events[0].Kind != "down" {
		t.Fatalf("expected 1 down event for unreachable health URL, got %+v", n.events)
	}
	if !strings.HasPrefix(n.events[0].E.Reason, "health probe GET ") {
		t.Fatalf("down event reason must describe the probe failure: %+v", n.events[0].E.Reason)
	}
}

func TestMonitor_HealthURL_NotProbedWhenEmpty(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer server.Close()
	svc := &store.Service{Name: "svc_health5", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	m.Tick(ctx)
	m.Tick(ctx)
	if hits != 0 {
		t.Fatalf("service without health_url must not be probed, got %d requests", hits)
	}
	if len(n.events) != 0 {
		t.Fatalf("expected no alerts, got %+v", n.events)
	}
}

func TestMonitor_HealthURL_ContainerDownStillDown(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	svc := &store.Service{Name: "svc_health6", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok", HealthURL: server.URL}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "exited", ExitCode: 1}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	m.Tick(ctx)
	m.Tick(ctx)
	if len(n.events) != 1 || n.events[0].Kind != "down" {
		t.Fatalf("expected 1 down event despite healthy probe, got %+v", n.events)
	}
	if !strings.Contains(n.events[0].E.Reason, "exited") {
		t.Fatalf("container-down reason must win over probe result: %+v", n.events[0].E.Reason)
	}
}

func TestMonitor_DownThenRecovery(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc2", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running", Digest: "sha256:abc"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 50*time.Millisecond)
	// Tick 1: up
	m.Tick(ctx)
	// capture digest from current container for assertion
	digest := f.Containers[svc.Name][0].Digest
	// Tick 2: down (first down observation)
	f.Containers[svc.Name][0].State = "exited"
	f.Containers[svc.Name][0].ExitCode = 1
	m.Tick(ctx)
	// Tick 3: still down (second consecutive tick) should trigger alert
	m.Tick(ctx)
	if len(n.events) != 1 {
		t.Fatalf("expected 1 down event, got %d: %v", len(n.events), n.events)
	}
	if n.events[0].Kind != "down" || n.events[0].E.Trigger != store.TriggerMonitor {
		t.Fatalf("first event must be down with monitor trigger: %+v", n.events[0])
	}
	if n.events[0].E.Digest != digest {
		t.Fatalf("down event digest mismatch: got %q want %q", n.events[0].E.Digest, digest)
	}
	// Tick 4: recover
	f.Containers[svc.Name][0].State = "running"
	m.Tick(ctx)
	if len(n.events) != 2 {
		t.Fatalf("expected 1 recovery event, total 2, got %d: %v", len(n.events), n.events)
	}
	if n.events[1].Kind != "recovered" || n.events[1].E.Trigger != store.TriggerMonitor {
		t.Fatalf("second event must be recovered with monitor trigger: %+v", n.events[1])
	}
}

func TestMonitor_TransientDownThenUp_NoEvents(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc3", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	// Tick 1: up
	m.Tick(ctx)
	// Tick 2: down (first consecutive)
	f.Containers[svc.Name][0].State = "exited"
	m.Tick(ctx)
	// Tick 3: up (not a sustained down)
	f.Containers[svc.Name][0].State = "running"
	m.Tick(ctx)
	if len(n.events) != 0 {
		t.Fatalf("expected zero events for transient down; got %d: %+v", len(n.events), n.events)
	}
}

func TestMonitor_DownWithUnhealthyCountsAsDown(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc4", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	// Health unhealthy on first tick
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running", Health: "unhealthy"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 10*time.Millisecond)
	// Tick 1: down due to unhealthy health
	m.Tick(ctx)
	// Tick 2: still down
	f.Containers[svc.Name][0].State = "exited"
	f.Containers[svc.Name][0].ExitCode = 1
	m.Tick(ctx)
	if len(n.events) != 1 || n.events[0].Kind != "down" {
		t.Fatalf("expected 1 down event due to unhealthy health; got: %+v", n.events)
	}
}

func TestMonitor_ZeroContainersCountsDown(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc5", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 10*time.Millisecond)
	m.Tick(ctx) // tick 1: no containers
	f.Containers[svc.Name] = []docker.Container{{ID: "c1", Name: "web", State: "exited"}}
	m.Tick(ctx) // tick 2: still no containers -> should count as down
	if len(n.events) != 1 || n.events[0].Kind != "down" {
		t.Fatalf("expected 1 down event for zero containers; got: %+v", n.events)
	}
}

func TestMonitor_BootBaselineDownFromStart(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc6", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	// Boot: down from start
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "exited"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	m.Tick(ctx) // tick 1: down
	m.Tick(ctx) // tick 2: down again -> should trigger one down event
	if len(n.events) != 1 || n.events[0].Kind != "down" {
		t.Fatalf("expected 1 down event after boot baseline; got: %+v", n.events)
	}
}

func TestMonitor_SuppressionAcrossEpisodes(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc7", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	// Tick 1: up
	m.Tick(ctx)
	// Episode 1: two consecutive down observations -> one down alert
	f.Containers[svc.Name][0].State = "exited"
	f.Containers[svc.Name][0].ExitCode = 1
	m.Tick(ctx)
	m.Tick(ctx)
	// Up -> recovery alert
	f.Containers[svc.Name][0].State = "running"
	m.Tick(ctx)
	// Episode 2 within the cooldown: two down observations -> suppressed
	f.Containers[svc.Name][0].State = "exited"
	m.Tick(ctx)
	m.Tick(ctx)
	// Up again -> no recovery, since episode 2 never alerted
	f.Containers[svc.Name][0].State = "running"
	m.Tick(ctx)
	// Expect only 2 events (1 down, 1 recovered)
	if len(n.events) != 2 {
		t.Fatalf("expected 2 events total (down + recovered) after suppression test; got %d: %+v", len(n.events), n.events)
	}
	if n.events[0].Kind != "down" || n.events[1].Kind != "recovered" {
		t.Fatalf("expected down then recovered, got %+v", n.events)
	}
}

// TestMonitor_AlertsAreNotGatedByLegacyMode locks in that the monitor
// forwards events unconditionally: per-event suppression lives in the
// notifier chain (notify.Route), not here. A stored legacy notify_mode must
// not gate it.
func TestMonitor_AlertsAreNotGatedByLegacyMode(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc8", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	// Legacy mode "never" is stored; the monitor must still forward.
	if err := st.SetSetting(ctx, store.SettingNotifyMode, "never"); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "exited"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 10*time.Millisecond)
	m.Tick(ctx)
	m.Tick(ctx)
	if len(n.events) != 1 || n.events[0].Kind != "down" {
		t.Fatalf("monitor must forward alerts regardless of stored mode; got %+v", n.events)
	}
}

func TestMonitor_DockerErrorDoesNotAffectState(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "svc10", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := &docker.Fake{Containers: map[string][]docker.Container{svc.Name: {{ID: "c1", Name: "web", State: "running"}}}}
	n := &capturingNotifier{}
	m := New(st, f, n, 20*time.Millisecond)
	m.Tick(ctx) // baseline: up
	// An errored tick must leave state untouched and emit nothing.
	f.Err = errors.New("boom")
	m.Tick(ctx)
	f.Err = nil
	// The errored tick must not have counted as a down observation, so the
	// first real down tick below is observation #1 and does not alert.
	f.Containers[svc.Name][0].State = "exited"
	m.Tick(ctx)
	if len(n.events) != 0 {
		t.Fatalf("docker error should not emit events; got %+v", n.events)
	}
}
