package store

import (
	"context"
	"database/sql"
	"testing"
)

// PRAGMAs must be configured per connection, not per *sql.DB. Since sql.Open
// returns a pool, a `db.Exec("PRAGMA ...")` only reaches whichever connection
// the pool happened to hand out, leaving every later connection on the SQLite
// defaults: busy_timeout=0 (instant SQLITE_BUSY under concurrent writes) and
// foreign_keys=OFF (ON DELETE CASCADE silently does not fire).
func TestOpenAppliesPragmasToEveryPooledConnection(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// Hold the connections open so the pool is forced to create distinct ones
	// rather than handing back the same configured connection each time.
	const n = 4
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := st.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		t.Cleanup(func() { c.Close() })
		conns = append(conns, c)
	}

	for i, c := range conns {
		var busyTimeout, foreignKeys int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d: read busy_timeout: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d: read foreign_keys: %v", i, err)
		}
		if busyTimeout != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000", i, busyTimeout)
		}
		if foreignKeys != 1 {
			t.Errorf("conn %d: foreign_keys = %d, want 1", i, foreignKeys)
		}
	}
}

// The default DBPath is relative ("deploybot.db"), so the DSN must not turn the
// filename into a URI authority.
func TestOpenAcceptsRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())

	st, err := Open("deploybot.db", make([]byte, 32))
	if err != nil {
		t.Fatalf("Open with relative path: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	svc := &Service{Name: "app", WatchedImage: "img", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	got, err := st.GetService(ctx, svc.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if got.Name != "app" {
		t.Errorf("Name = %q, want %q", got.Name, "app")
	}
}

// The user-visible consequence of foreign_keys being off: deleting a service
// must cascade to its deployments instead of orphaning them.
func TestDeleteServiceCascadesToDeployments(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// Occupy the first connection for the whole test so the work below runs on
	// a second one, the way a concurrent web request and executor goroutine do.
	held, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer held.Close()

	svc := &Service{Name: "app", WatchedImage: "img", Policy: PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	dep := &Deployment{ServiceID: svc.ID, Trigger: TriggerManual, TargetDigest: "sha256:x", Status: DeployPending}
	if err := st.CreateDeployment(ctx, dep); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if err := st.DeleteService(ctx, svc.ID); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}

	var orphans int
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM deployment WHERE service_id = ?", svc.ID).Scan(&orphans); err != nil {
		t.Fatalf("count deployments: %v", err)
	}
	if orphans != 0 {
		t.Errorf("deleting a service left %d orphaned deployment(s), want 0", orphans)
	}
}
