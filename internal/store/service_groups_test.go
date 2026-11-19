package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestReturningGroupKeepsDashboardSettingsAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "groups.db")
	st, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &Service{Name: "api", Group: "Platform", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	initial, err := st.ListServiceGroups(ctx)
	if err != nil || len(initial) != 2 {
		t.Fatalf("initial groups = %+v, %v", initial, err)
	}
	platform := initial[1].ID
	if err := st.MoveServiceGroup(ctx, platform, initial[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetServiceGroupCollapsed(ctx, platform, true); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteService(ctx, svc.ID); err != nil {
		t.Fatal(err)
	}
	if groups, err := st.ListServiceGroups(ctx); err != nil || len(groups) != 1 || groups[0].Name != DefaultServiceGroup {
		t.Fatalf("groups with no members = %+v, %v", groups, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.CreateService(ctx, &Service{Name: "worker", Group: "Platform", Policy: PolicyManual}); err != nil {
		t.Fatal(err)
	}
	groups, err := reopened.ListServiceGroups(ctx)
	if err != nil || len(groups) != 2 || groups[0].ID != platform || !groups[0].Collapsed || groups[1].Name != DefaultServiceGroup {
		t.Fatalf("groups after reopening and rejoining = %+v, %v", groups, err)
	}
}

func TestStaleServiceSavePreservesGroup(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "api", Group: "Platform", Policy: PolicyManual}
	if err := st.SaveServiceConfig(ctx, svc, nil, nil); err != nil {
		t.Fatal(err)
	}
	stale := *svc
	svc.Group = "Applications"
	if err := st.SaveServiceConfig(ctx, svc, nil, &stale); err != nil {
		t.Fatal(err)
	}
	candidate := stale
	candidate.Group = "Stale group"
	if err := st.SaveServiceConfig(ctx, &candidate, nil, &stale); !errors.Is(err, ErrServiceConflict) {
		t.Fatalf("stale save returned %v", err)
	}
	got, err := st.GetService(ctx, svc.ID)
	if err != nil || got.Group != "Applications" {
		t.Fatalf("service after stale save = %+v, %v", got, err)
	}
	groups, err := st.ListServiceGroups(ctx)
	if err != nil || len(groups) != 2 || groups[0].Name != DefaultServiceGroup || groups[1].Name != "Applications" {
		t.Fatalf("groups after stale save = %+v, %v", groups, err)
	}
}
