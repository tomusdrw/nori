package web

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"nori/internal/store"
)

func TestSaveOrdinaryServiceSharesDashboardAndTemplateWrites(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server := &Server{store: st}
	ctx := context.Background()

	dashboardEnv := "TOKEN=first\n"
	svc := &store.Service{Name: "app", WatchedImage: "nginx:latest", Policy: store.PolicyManual, DeployScript: "true"}
	if _, err := server.saveOrdinaryService(ctx, ordinaryWrite{Service: svc, Environment: &dashboardEnv, EnvironmentMode: dashboardEnvironment}); err != nil {
		t.Fatal(err)
	}
	previous := *svc
	template := "TOKEN='[REDACTED]'\n"
	svc.DeployScript = "echo updated"
	if _, err := server.saveOrdinaryService(ctx, ordinaryWrite{Service: svc, Previous: &previous, Environment: &template, EnvironmentMode: templateEnvironment}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetEnvFile(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "TOKEN=\"first\"\n" {
		t.Fatalf("template write changed preserved secret: %q", got)
	}
}

func TestServiceCreateDuplicateNameExplainsConflictAndPreservesEnvironment(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	existing := &store.Service{Name: "app", WatchedImage: "nginx:latest", Policy: store.PolicyManual, DeployScript: "true"}
	if _, err := (&Server{store: st}).saveOrdinaryService(context.Background(), ordinaryWrite{Service: existing, EnvironmentMode: dashboardEnvironment}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/services", strings.NewReader("name=app&watched_image=nginx%3Alatest&policy=manual&deploy_script=true&env_file=TOKEN%3Dtyped%0A"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	(&Server{store: st}).handleServiceCreate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "already exists") || !strings.Contains(body, "TOKEN=typed") {
		t.Fatalf("duplicate response should explain the name conflict and preserve environment: %s", body)
	}
}

func TestServiceUpdateRejectsStaleGenerationWithoutRenderingSubmittedEnvironment(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "nginx:latest", Policy: store.PolicyManual, DeployScript: "true"}
	env := "TOKEN=current\n"
	if err := st.SaveServiceConfig(ctx, svc, &env, nil); err != nil {
		t.Fatal(err)
	}
	staleVersion := svc.ConfigVersion
	newer := *svc
	newer.DeployScript = "echo current"
	if err := st.SaveServiceConfig(ctx, &newer, nil, svc); err != nil {
		t.Fatal(err)
	}

	server := &Server{store: st}
	body := "name=app&watched_image=nginx%3Alatest&policy=manual&deploy_script=echo+stale&env_file=TOKEN%3Dstale&expected_config_version=" + strconv.FormatInt(staleVersion, 10)
	req := httptest.NewRequest(http.MethodPost, "/services/app", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "app")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	server.handleServiceUpdate(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusConflict, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "Reload configuration") || !strings.Contains(body, "TOKEN=current") || strings.Contains(body, "TOKEN=stale") {
		t.Fatalf("conflict response must preserve persisted environment without submitted environment: %s", body)
	}
}

func TestServiceUpdateRetryPreservesSubmittedGenerationAfterStoreFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nori.db")
	st, err := store.Open(dbPath, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "nginx:latest", Policy: store.PolicyManual, DeployScript: "true"}
	env := "TOKEN=current\n"
	if err := st.SaveServiceConfig(ctx, svc, &env, nil); err != nil {
		t.Fatal(err)
	}
	staleVersion := svc.ConfigVersion
	current := *svc
	current.DeployScript = "echo current"
	if err := st.SaveServiceConfig(ctx, &current, nil, svc); err != nil {
		t.Fatal(err)
	}
	lockedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	lockedDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = lockedDB.Close() })
	if _, err := lockedDB.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = lockedDB.Exec("ROLLBACK") })

	body := "name=app&watched_image=nginx%3Alatest&policy=manual&deploy_script=echo+stale&env_file=TOKEN%3Dstale&expected_config_version=" + strconv.FormatInt(staleVersion, 10)
	req := httptest.NewRequest(http.MethodPost, "/services/app", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "app")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	(&Server{store: st}).handleServiceUpdate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, `name="expected_config_version" value="1"`) || !strings.Contains(body, "TOKEN=current") || strings.Contains(body, "TOKEN=stale") {
		t.Fatalf("retry response must preserve submitted generation and persisted environment: %s", body)
	}
}

func TestLegacyServiceNameCanBeUpdated(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	legacy := &store.Service{Name: "Legacy Service", WatchedImage: "nginx:latest", Policy: store.PolicyManual, DeployScript: "true"}
	if err := st.CreateService(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	candidate := *legacy
	candidate.DeployScript = "echo updated"
	previous := *legacy
	if _, err := (&Server{store: st}).saveOrdinaryService(ctx, ordinaryWrite{
		Service: &candidate, Previous: &previous, EnvironmentMode: dashboardEnvironment,
	}); err != nil {
		t.Fatalf("legacy service update failed: %v", err)
	}
	updated, err := st.GetService(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DeployScript != "echo updated" {
		t.Fatalf("deploy script = %q, want updated script", updated.DeployScript)
	}
}

func TestCrossSurfaceWritersAllowOneGenerationWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nori.db")
	first, err := store.Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := store.Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	ctx := context.Background()
	service := &store.Service{Name: "app", WatchedImage: "nginx:latest", Policy: store.PolicyManual, DeployScript: "true"}
	if _, err := (&Server{store: first}).saveOrdinaryService(ctx, ordinaryWrite{Service: service, EnvironmentMode: dashboardEnvironment}); err != nil {
		t.Fatal(err)
	}
	fromMCP, err := second.GetService(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	dashboardCandidate := *service
	dashboardCandidate.DeployScript = "echo dashboard"
	mcpCandidate := *fromMCP
	mcpCandidate.HealthURL = "https://example.com/health"
	dashboardExpected := *service
	mcpExpected := *fromMCP

	start := make(chan struct{})
	results := make(chan error, 2)
	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		<-start
		_, err := (&Server{store: first}).saveOrdinaryService(ctx, ordinaryWrite{Service: &dashboardCandidate, Previous: &dashboardExpected, EnvironmentMode: dashboardEnvironment})
		results <- err
	}()
	go func() {
		defer writers.Done()
		<-start
		_, err := (&Server{store: second}).saveOrdinaryService(ctx, ordinaryWrite{Service: &mcpCandidate, Previous: &mcpExpected, EnvironmentMode: templateEnvironment})
		results <- err
	}()
	close(start)
	writers.Wait()
	close(results)

	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrServiceConflict):
			conflicts++
		default:
			t.Fatalf("writer error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("writers: %d successes, %d conflicts; want one of each", successes, conflicts)
	}
	got, err := first.GetService(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigVersion != 2 {
		t.Fatalf("final config version = %d, want 2", got.ConfigVersion)
	}
}

func TestDashboardServiceDescription(t *testing.T) {
	st, _, srv, cookies, _ := newSelfServiceServer(t)
	ctx := context.Background()
	newPage := getAuthed(t, srv, cookies, "/services/new")
	form := url.Values{
		"csrf_token": {csrfFromBody(newPage)}, "name": {"described"},
		"watched_image": {"nginx:latest"}, "policy": {"manual"}, "deploy_script": {"true"},
		"description": {"Public API <production>\r\nBehind Caddy on port 8080"},
	}
	postAuthed(t, srv, cookies, "/services", form.Encode())
	for _, path := range []string{"/services/described", "/services/described/edit"} {
		body := getAuthed(t, srv, cookies, path)
		if !strings.Contains(body, "Public API &lt;production&gt;\nBehind Caddy on port 8080") || strings.Contains(body, "<production>") {
			t.Fatalf("%s must render the saved description as escaped multiline text: %s", path, body)
		}
	}
	for _, description := range []string{"Private API; staging", ""} {
		svc, err := st.GetServiceByName(ctx, "described")
		if err != nil {
			t.Fatal(err)
		}
		form.Set("expected_config_version", strconv.FormatInt(svc.ConfigVersion, 10))
		form.Set("description", description)
		postAuthed(t, srv, cookies, "/services/described", form.Encode())
		svc, err = st.GetServiceByName(ctx, "described")
		if err != nil {
			t.Fatal(err)
		}
		if svc.Description != description {
			t.Fatalf("saved description = %q, want %q", svc.Description, description)
		}
	}
}
