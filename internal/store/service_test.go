package store

import (
	"context"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestServiceCRUD(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := &Service{Name: "blog", WatchedImage: "ghcr.io/me/blog:latest", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if svc.ID == 0 {
		t.Fatal("ID not set")
	}
	if svc.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not set")
	}

	got, err := st.GetService(ctx, svc.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Name != "blog" || got.WatchedImage != "ghcr.io/me/blog:latest" || got.Policy != PolicyManual {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	list, err := st.ListServices(ctx)
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
}

func TestGetService_NotFound(t *testing.T) {
	st := testStore(t)
	if _, err := st.GetService(context.Background(), 999); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
