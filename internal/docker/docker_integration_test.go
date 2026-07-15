//go:build integration

package docker

import (
	"context"
	"testing"
	"time"
)

// Requires a running Docker daemon. Run with: go test -tags integration ./internal/docker/
func TestListByService_Integration(t *testing.T) {
	c, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := c.ListByService(ctx, "no-such-service-xyz")
	if err != nil {
		t.Fatalf("ListByService: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}
