package poller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"nori/internal/executor"
	"nori/internal/store"
)

func TestSwitchToManualScriptStopsImagePolling(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	svc := &store.Service{Name: "job", WatchedImage: "nginx:stable", Policy: store.PolicyManual, DeployScript: "true"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := New(st, func(_ context.Context, image string) (string, error) {
		calls++
		if image != "nginx:stable" {
			t.Errorf("unexpected lookup for %q", image)
		}
		return "sha256:abc", nil
	}, nil, time.Minute)
	p.Tick(ctx)
	if calls != 1 || p.CachedDigest(svc.Name) != "sha256:abc" {
		t.Fatal("expected the image digest to be polled and cached under the manual policy")
	}
	svc.DeploymentMode = store.DeploymentModeManualScript
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	p.Tick(ctx)
	if calls != 1 || p.CachedDigest(svc.Name) != "" {
		t.Fatalf("after conversion: %d registry calls in total, cached digest %q", calls, p.CachedDigest(svc.Name))
	}
}

func TestImmediateDeploymentRetriesAfterBackup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := &store.Service{Name: "app", WatchedImage: "nginx:stable", Policy: store.PolicyImmediate, DeployScript: "true"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:old"
	latest := func(context.Context, string) (string, error) { return digest, nil }
	ex := executor.New(st, executor.OSRunner{}, latest, 0)
	p := New(st, latest, ex, time.Minute)
	p.Tick(ctx)
	release, err := ex.BeginServiceOperation(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest = "sha256:new"
	p.Tick(ctx)
	jobs, err := st.ListDeployments(ctx, svc.ID, 10)
	release()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("deployed during backup: %v %v", jobs, err)
	}
	p.Tick(ctx)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		jobs, err := st.ListDeployments(ctx, svc.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 1 && jobs[0].Status == store.DeploySuccess {
			if jobs[0].TargetDigest != digest {
				t.Fatalf("deployed digest %s", jobs[0].TargetDigest)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("previously blocked deployment was lost")
}
