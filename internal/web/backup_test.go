package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nori/internal/auth"
	"nori/internal/backup"
	"nori/internal/store"
)

func TestBackupDownloadAndUploadToAnotherInstance(t *testing.T) {
	ctx := context.Background()
	t.Setenv("NORI_BACKUP_DIR", "")
	t.Setenv("NORI_BACKUP_RECIPIENT", "")
	hash, err := auth.HashPassword("test")
	if err != nil {
		t.Fatal(err)
	}
	setup := func(version byte) (*store.Store, *backup.Manager, *Server, []*http.Cookie, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "nori.db")
		key := make([]byte, 32)
		key[0] = version
		st, err := store.Open(path, key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		a, err := auth.New(hash, key)
		if err != nil {
			t.Fatal(err)
		}
		manager, err := backup.New(ctx, st, nil, nil, path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.SaveDiskPolicy(ctx, backup.DiskPolicy{}); err != nil {
			t.Fatal(err)
		}
		srv := NewServer(st, nil, nil, nil, a, Channels{})
		srv.SetBackups(manager)
		cookies := loginCookies(t, srv)
		csrf := csrfFromBody(getAuthed(t, srv, cookies, "/backups"))
		return st, manager, srv, cookies, csrf
	}

	src, manager, srv, cookies, csrf := setup(1)
	service := &store.Service{Name: "original", DeployScript: "true", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript}
	env := "# configuration comment\nPASSWORD=source-private-value\n"
	if err := src.SaveServiceConfig(ctx, service, &env, nil); err != nil {
		t.Fatal(err)
	}
	stopCtx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { defer close(stopped); manager.Run(stopCtx) }()
	t.Cleanup(func() { stop(); <-stopped })
	postAuthed(t, srv, cookies, "/services/original/backups/create", url.Values{"csrf_token": {csrf}}.Encode())
	var job store.BackupJob
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		jobs, err := src.BackupHistory(ctx, service.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 1 && (jobs[0].Status == "complete" || jobs[0].Status == "failed") {
			job = jobs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "complete" {
		t.Fatalf("manual copy did not complete: %s %s", job.Status, job.Message)
	}
	file := backupJobURL(job.ID) + "/download"
	unauthorized := httptest.NewRecorder()
	srv.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, file, nil))
	if unauthorized.Code == http.StatusOK {
		t.Fatal("unauthenticated download succeeded")
	}
	archive := backupTestRequest(srv, cookies, http.MethodGet, file, "", nil)
	if archive.Code != http.StatusOK || bytes.Contains(archive.Body.Bytes(), []byte("source-private-value")) {
		t.Fatal("download missing or exposes dotenv")
	}

	dst, _, destSrv, destCookies, destCSRF := setup(2)
	upload := func(csrf string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if err := mw.WriteField("csrf_token", csrf); err != nil {
			t.Fatal(err)
		}
		if err := mw.WriteField("identity", manager.RecoveryKey()); err != nil {
			t.Fatal(err)
		}
		part, err := mw.CreateFormFile("archive", "copy.nori")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(archive.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		return backupTestRequest(destSrv, destCookies, http.MethodPost, "/backups/import", mw.FormDataContentType(), &body)
	}
	if res := upload("invalid"); res.Code != http.StatusForbidden {
		t.Fatalf("wrong CSRF token allowed upload: %d", res.Code)
	}
	review := upload(destCSRF)
	if review.Code != http.StatusSeeOther {
		t.Fatalf("could not stage upload: %d %s", review.Code, review.Body.String())
	}
	location := review.Header().Get("Location")
	body := getAuthed(t, destSrv, destCookies, location)
	if !strings.Contains(body, "Restore original") || strings.Contains(body, "source-private-value") {
		t.Fatal("invalid import review")
	}
	newName := "restored-" + strconv.FormatInt(job.ID, 10)
	postAuthed(t, destSrv, destCookies, location, url.Values{"csrf_token": {destCSRF}, "name": {newName}}.Encode())
	restored, err := dst.GetServiceByName(ctx, newName)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dst.GetEnvFile(ctx, restored.ID)
	if err != nil || got != env || restored.Policy != store.PolicyManual {
		t.Fatalf("restored configuration invalid: %v", err)
	}
}

func backupTestRequest(srv *Server, cookies []*http.Cookie, method, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}
