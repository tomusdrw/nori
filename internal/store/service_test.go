package store

import (
	"context"
	"database/sql"
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

func TestEnsureSelfServiceIsManagedAndKeepsPolicy(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	self, err := st.EnsureSelfService(ctx, "ghcr.io/acme/deploybot:latest")
	if err != nil {
		t.Fatal(err)
	}
	if !self.IsSelf || self.Name != SelfServiceName || self.Policy != PolicyManual {
		t.Fatalf("unexpected self service: %+v", self)
	}
	if self.DeployScript != SelfDeployScript {
		t.Fatal("self service did not receive managed handoff script")
	}
	self.Policy = PolicyScheduled
	self.CronExpr = "0 3 * * *"
	if err := st.UpdateService(ctx, self); err != nil {
		t.Fatal(err)
	}
	again, err := st.EnsureSelfService(ctx, "ghcr.io/acme/deploybot:v2")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != self.ID || again.Policy != PolicyScheduled || again.WatchedImage != "ghcr.io/acme/deploybot:v2" {
		t.Fatalf("self service was not safely refreshed: %+v", again)
	}
}

func TestOpenMigratesExistingServiceTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE service (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, watched_image TEXT NOT NULL,
		policy TEXT NOT NULL, cron_expr TEXT NOT NULL DEFAULT '', deploy_script TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.EnsureSelfService(context.Background(), "image"); err != nil {
		t.Fatalf("migration did not add is_self: %v", err)
	}
}
