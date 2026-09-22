package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"nori/internal/auth"
	"nori/internal/docker"
	"nori/internal/executor"
	"nori/internal/poller"
	"nori/internal/store"
)

func TestHealthz_Unauthenticated(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "healthz.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	latest := func(context.Context, string) (string, error) { return "", nil }
	ex := executor.New(st, &executor.OSRunner{}, latest, 0)
	pl := poller.New(st, latest, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz without session: status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "ok" {
		t.Fatalf("healthz body = %q, want %q", body, "ok")
	}
}
