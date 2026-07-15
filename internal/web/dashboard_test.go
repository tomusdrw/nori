package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"deploybot/internal/docker"
	"deploybot/internal/store"
)

func TestDashboard_RendersServiceWithUpdate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := &store.Service{Name: "blog", WatchedImage: "ghcr.io/me/blog:latest", Policy: store.PolicyManual}
	if err := st.CreateService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}

	dk := &docker.Fake{Containers: map[string][]docker.Container{
		"blog": {{Name: "blog-web", Image: "ghcr.io/me/blog:latest", Digest: "sha256:running0000", State: "running"}},
	}}
	latest := func(ctx context.Context, image string) (string, error) { return "sha256:newer00000000", nil }

	srv := NewServer(st, dk, latest)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"blog", "running", "update available"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}
