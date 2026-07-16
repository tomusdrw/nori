package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		"blog": {{ID: "blog-web", Name: "blog-web", Image: "ghcr.io/me/blog:latest", Digest: "sha256:running0000", State: "running", StartedAt: time.Now().Add(-2 * time.Hour)}},
	}, LogData: map[string]string{"blog-web": "booted\nlistening on :8080\nrequest complete"}}
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
	for _, want := range []string{"blog", "running", "update available", ">Update<", "Running for", "Last deploy", "Recent logs", "listening on :8080", "Open full logs"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestServiceDetail_NoUpdateWhenDigestsMatch(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := &store.Service{Name: "blog", WatchedImage: "ghcr.io/me/blog:latest", Policy: store.PolicyManual}
	if err := st.CreateService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}

	// The running container and the registry report the exact same digest, so
	// no update should be advertised.
	const digest = "sha256:bc3a491edb7d0000000000000000000000000000000000000000000000000000"
	dk := &docker.Fake{Containers: map[string][]docker.Container{
		"blog": {{Name: "blog-web", Image: "ghcr.io/me/blog:latest", Digest: digest, State: "running"}},
	}}
	latest := func(ctx context.Context, image string) (string, error) { return digest, nil }
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

	req := httptest.NewRequest(http.MethodGet, "/services/blog", nil)
	for _, c := range loginRR.Result().Cookies() {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, "Update available") {
		t.Errorf("detail page advertises an update despite identical digests\n%s", body)
	} else if !strings.Contains(body, ">Re-deploy<") {
		t.Errorf("detail page should offer a subdued re-deploy action\n%s", body)
	}
}

func TestRecentLogsLimitsDashboardPreview(t *testing.T) {
	srv := &Server{docker: &docker.Fake{LogData: map[string]string{"one": "one\ntwo\nthree\nfour\nfive\nsix\nseven"}}}
	logs := srv.recentLogs(context.Background(), []docker.Container{{ID: "one", State: "running"}}, 3)
	if logs != "five\nsix\nseven" {
		t.Fatalf("recent logs = %q", logs)
	}
}
