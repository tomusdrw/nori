package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"deploybot/internal/store"
)

type fakeRunner struct {
	err  error
	log  string
	called bool
}

func (f *fakeRunner) Run(ctx context.Context, script string, env []string, stdout, stderr io.Writer) error {
	f.called = true
	if f.log != "" {
		io.WriteString(stdout, f.log)
	}
	return f.err
}

func TestDeploy_Success(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyManual, DeployScript: "echo hi"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{log: "deployed\n"}
	ex := New(st, runner, func(ctx context.Context, image string) (string, error) {
		return "sha256:new", nil
	}, 0)

	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// wait for async deploy
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, err := st.GetDeployment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != store.DeployRunning {
			if d.Status != store.DeploySuccess {
				t.Fatalf("status = %s", d.Status)
			}
			if !strings.Contains(d.Log, "deployed") {
				t.Fatalf("log = %q", d.Log)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deploy did not finish")
}

func TestDeploy_FailureCooldown(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyImmediate, DeployScript: "exit 1"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{err: errors.New("boom")}
	latest := func(ctx context.Context, image string) (string, error) { return "sha256:bad", nil }
	ex := New(st, runner, latest, time.Minute)

	if id, err := ex.Deploy(ctx, svc.ID, store.TriggerAuto); err != nil || id == 0 {
		t.Fatalf("expected deploy to start, id=%d err=%v", id, err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := ex.Deploy(ctx, svc.ID, store.TriggerAuto); err == nil {
		t.Fatal("expected cooldown block")
	}
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
