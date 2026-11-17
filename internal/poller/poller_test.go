package poller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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
