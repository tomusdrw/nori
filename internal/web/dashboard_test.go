package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"deploybot/internal/auth"
	"deploybot/internal/docker"
	"deploybot/internal/executor"
	"deploybot/internal/poller"
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
	ex := executor.New(st, &executor.OSRunner{}, latest, 0)
	pl := poller.New(st, latest, ex, 0)
	pl.Tick(context.Background())

	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	srv := NewServer(st, dk, ex, pl, a)

	loginRR := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=test"))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.ServeHTTP(loginRR, loginReq)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range loginRR.Result().Cookies() {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"blog", "running", "update available"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}
