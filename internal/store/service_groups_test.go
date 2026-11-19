package store

import (
	"context"
	"errors"
	"testing"
)

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
