package web

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"nori/internal/auth"
	"nori/internal/backup"
	"nori/internal/mcpauth"
	"nori/internal/store"
)

func TestS3SettingsSaveAndRetry(t *testing.T) {
	ctx := context.Background()
	t.Setenv("NORI_BACKUP_DIR", "")
	t.Setenv("NORI_BACKUP_RECIPIENT", "")
	path := filepath.Join(t.TempDir(), "nori.db")
	key := make([]byte, 32)
	st, err := store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, err := auth.HashPassword("test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(hash, key)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(ctx, st, nil, nil, path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(st, nil, nil, nil, a, Channels{})
	srv.SetBackups(manager)
	cookies := loginCookies(t, srv)
	page := getAuthed(t, srv, cookies, "/backups")
	form := url.Values{
		"csrf_token":        {csrfFromBody(page)},
		"bucket":            {"private-archives"},
		"region":            {"us-west-2"},
		"prefix":            {"backups/server"},
		"access_key_id":     {"example-key-id"},
		"secret_access_key": {"private-access-key"},
		"read_only":         {"on"},
	}
	submit := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/backups/s3", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", accept)
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if secret := form.Get("secret_access_key"); secret != "" && strings.Contains(rr.Body.String(), secret) {
			t.Fatal("save response exposed the secret access key")
		}
		return rr
	}

	if res := submit("application/json"); res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"field":"endpoint"`) {
		t.Fatalf("configuring a bucket without an endpoint should flag the endpoint field, got %d", res.Code)
	}
	const initialEndpoint = "https://s3.us-west-2.amazonaws.com"
	form.Set("endpoint", initialEndpoint)
	form.Set("region", initialEndpoint)
	rejected := submit("application/json")
	var feedback struct {
		Field   string
		Message string
	}
	if rejected.Code != http.StatusBadRequest || json.Unmarshal(rejected.Body.Bytes(), &feedback) != nil ||
		feedback.Field != "region" || !strings.Contains(feedback.Message, "us-west-2") ||
		rejected.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected actionable inline region error, got status %d", rejected.Code)
	}
	if saved, err := manager.S3(ctx); err != nil || saved.Bucket != "" {
		t.Fatal("invalid settings changed the saved bucket")
	}

	fallback := submit("text/html")
	if fallback.Code != http.StatusBadRequest || !strings.HasPrefix(fallback.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected styled failure page, got status %d and type %q", fallback.Code, fallback.Header().Get("Content-Type"))
	}
	for _, want := range []string{"alert-error", "private-archives", "backups/server", "example-key-id",
		initialEndpoint, `name="read_only" type="checkbox" checked`, "Re-enter the secret access key"} {
		if !strings.Contains(fallback.Body.String(), want) {
			t.Errorf("failure page missing %q", want)
		}
	}

	form.Set("region", "us-west-2")
	success := submit("application/json")
	const savedURL = "/backups?s3_saved=1#s3-storage"
	var savedFeedback struct{ Location string }
	if success.Code != http.StatusOK || json.Unmarshal(success.Body.Bytes(), &savedFeedback) != nil || savedFeedback.Location != savedURL {
		t.Fatalf("expected save confirmation URL, got status %d", success.Code)
	}
	confirmation := getAuthed(t, srv, cookies, "/backups?s3_saved=1")
	if !strings.Contains(confirmation, "S3 settings saved") || strings.Contains(confirmation, form.Get("secret_access_key")) {
		t.Fatal("save confirmation missing or exposed the secret access key")
	}
	form.Del("secret_access_key")
	form.Set("region", "eu-central-1")
	if res := submit("application/json"); res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"field":"secret_access_key"`) {
		t.Fatalf("changing region without a matching secret should flag the secret field, got %d", res.Code)
	}
	form.Set("region", "us-west-2")
	form.Set("endpoint", "https://s3.example.test")
	if res := submit("application/json"); res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"field":"secret_access_key"`) {
		t.Fatalf("changing endpoint should require the secret before saving, got %d", res.Code)
	}
	form.Set("endpoint", "/")
	if res := submit("application/json"); res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"field":"endpoint"`) {
		t.Fatalf("invalid custom endpoint should flag the URL field, got %d", res.Code)
	}
	form.Set("endpoint", initialEndpoint)
	form.Set("disabled", "on")
	if res := submit("text/html"); res.Code != http.StatusSeeOther || res.Header().Get("Location") != savedURL {
		t.Fatalf("expected successful redirect after pausing, got %d", res.Code)
	}
	saved, err := manager.S3(ctx)
	if err != nil || !saved.Disabled || !saved.ReadOnly || saved.SecretAccessKey != "private-access-key" {
		t.Fatal("saving unchanged credentials did not preserve the stored secret")
	}

	form.Set("endpoint", "https://s3.example.test")
	form.Set("virtual_hosted", "on")
	form.Set("secret_access_key", "private-s3-key")
	if res := submit("application/json"); res.Code != http.StatusOK {
		t.Fatalf("expected custom endpoint save, got %d", res.Code)
	}
	saved, err = manager.S3(ctx)
	if err != nil || saved.Endpoint != form.Get("endpoint") || !saved.VirtualHosted || saved.SecretAccessKey != "private-s3-key" {
		t.Fatal("custom endpoint settings were not stored")
	}
	settings := getAuthed(t, srv, cookies, "/backups")
	if !strings.Contains(settings, `value="https://s3.example.test"`) || strings.Contains(settings, form.Get("secret_access_key")) {
		t.Fatal("custom S3 settings did not render correctly")
	}

	browse := backupTestRequest(srv, cookies, http.MethodGet, "/backups/s3", "", nil)
	if browse.Code != http.StatusBadGateway || !strings.HasPrefix(browse.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(browse.Body.String(), "Edit S3 settings") {
		t.Fatalf("expected styled browse error with a way back to settings, got %d", browse.Code)
	}
}

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
	page := getAuthed(t, srv, cookies, "/services/original/backups")
	_, commands, found := strings.Cut(page, `<details class="backup-commands"><summary>Advanced backup commands</summary>`)
	commands, _, closed := strings.Cut(commands, "</details>")
	if !found || !closed {
		t.Fatal("advanced backup commands should be collapsed by default")
	}
	for _, name := range []string{"quiesce", "resume", "export", "import"} {
		if !strings.Contains(commands, `name="`+name+`"`) {
			t.Errorf("missing %s field in advanced backup commands", name)
		}
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
	occupied := &store.Service{Name: "original", Policy: store.PolicyManual}
	if err := dst.CreateService(ctx, occupied); err != nil {
		t.Fatal(err)
	}
	bad := url.Values{"csrf_token": {destCSRF}, "name": {"original"}, "deploy_script": {"echo edited"}}
	rejected := backupTestRequest(destSrv, destCookies, http.MethodPost, location, "application/x-www-form-urlencoded", strings.NewReader(bad.Encode()))
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "echo edited") ||
		!strings.Contains(rejected.Body.String(), "a service with that name already exists") ||
		!strings.Contains(rejected.Body.String(), `aria-invalid="true"`) ||
		!strings.Contains(rejected.Body.String(), `aria-describedby="restore-error"`) ||
		!strings.Contains(rejected.Body.String(), `form="restore-cancel"`) {
		t.Fatalf("restore form did not retain the draft and input: status %d", rejected.Code)
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

	// File transfer and MCP tools use the same grant family, even when calls
	// arrive on separate connections.
	owner := mcpauth.Identity{ClientID: "integration", GrantFamily: "writer", Scope: "nori:read nori:write"}
	rangeRequest := httptest.NewRequest(http.MethodGet, mcpArchivePath+"?backup_id="+strconv.FormatInt(job.ID, 10), nil)
	rangeRequest.Header.Set("Range", "bytes=0-15")
	ranged := httptest.NewRecorder()
	srv.handleMCPBackupArchive(ranged, rangeRequest.WithContext(mcpauth.WithIdentity(ctx, owner)))
	if ranged.Code != http.StatusPartialContent || !bytes.Equal(ranged.Body.Bytes(), archive.Body.Bytes()[:16]) {
		t.Fatal("MCP download did not honor a range request")
	}
	uploadRequest := httptest.NewRequest(http.MethodPost, mcpArchivePath, bytes.NewReader(archive.Body.Bytes()))
	uploadRequest.Header.Set("Content-Type", "application/octet-stream")
	uploadRequest.Header.Set("Nori-Recovery-Key", manager.RecoveryKey())
	recorder := httptest.NewRecorder()
	destSrv.handleMCPBackupArchive(recorder, uploadRequest.WithContext(mcpauth.WithIdentity(ctx, owner)))
	var ticket struct {
		DraftID string `json:"draft_id"`
	}
	if recorder.Code != http.StatusCreated || json.Unmarshal(recorder.Body.Bytes(), &ticket) != nil || ticket.DraftID == "" {
		t.Fatalf("archive upload failed: %d: %s", recorder.Code, recorder.Body)
	}
	handler := destSrv.newMCPHandler()
	connect := func(handler http.Handler, identity mcpauth.Identity) *mcp.ClientSession {
		t.Helper()
		httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(w, r.WithContext(mcpauth.WithIdentity(r.Context(), identity)))
		}))
		t.Cleanup(httpServer.Close)
		client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		return session
	}
	callTool := func(session *mcp.ClientSession, name string, args map[string]any, wantError bool) string {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError != wantError {
			t.Fatalf("%s: expected tool error %t, got %+v, %v", name, wantError, result, err)
		}
		content, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	client := connect(handler, owner)
	sourceClient := connect(srv.newMCPHandler(), owner)
	localArgs := map[string]any{"backup_id": job.ID}
	downloadJSON := callTool(sourceClient, "get_backup_archive_download", localArgs, false)
	if !strings.Contains(downloadJSON, mcpArchivePath) {
		t.Fatal("download tool did not return the archive path")
	}
	prepared, err := sourceClient.CallTool(ctx, &mcp.CallToolParams{Name: "prepare_backup_restore", Arguments: localArgs})
	if err != nil || prepared.IsError {
		t.Fatalf("prepare local backup: %+v %v", prepared, err)
	}
	draftID, _ := prepared.StructuredContent.(map[string]any)["draft_id"].(string)
	callTool(sourceClient, "cancel_backup_restore", map[string]any{"draft_id": draftID}, false)
	other := owner
	other.GrantFamily = "another-grant-for-the-same-client"
	args := map[string]any{"draft_id": ticket.DraftID}
	callTool(connect(handler, other), "get_backup_restore_review", args, true)
	reviewJSON := callTool(client, "get_backup_restore_review", args, false)
	if strings.Contains(reviewJSON, "source-private-value") || !strings.Contains(reviewJSON, "original") {
		t.Fatal("MCP review must show service metadata without its archived environment values")
	}
	args = map[string]any{"draft_id": ticket.DraftID, "name": "restored-via-mcp", "review_commands": false}
	callTool(client, "confirm_backup_restore", args, true)
	args["review_commands"] = true
	callTool(client, "confirm_backup_restore", args, false)
	callTool(client, "confirm_backup_restore", args, true)
	mcpRestored, err := dst.GetServiceByName(ctx, "restored-via-mcp")
	if err != nil {
		t.Fatal(err)
	}
	got, err = dst.GetEnvFile(ctx, mcpRestored.ID)
	if err != nil || got != env {
		t.Fatal("MCP restore did not preserve the archived environment")
	}
	callTool(client, "get_backup_recovery_key", map[string]any{}, true)
	privileged := owner
	privileged.Scope += " " + mcpauth.ScopeBackupAdmin
	admin := connect(handler, privileged)
	keyJSON := callTool(admin, "get_backup_recovery_key", map[string]any{}, false)
	if !strings.Contains(keyJSON, destSrv.backups.manager.RecoveryKey()) {
		t.Fatal("authorized recovery key response must contain the exact key")
	}
	storage := map[string]any{"endpoint": "https://s3.example.test", "region": "us-east-1",
		"prefix": "nori", "bucket": "backups", "access_key_id": "example", "secret_access_key": "private-s3-value"}
	callTool(client, "set_backup_storage", storage, true)
	callTool(admin, "set_backup_storage", storage, false)
	storageJSON := callTool(admin, "get_backup_storage", map[string]any{}, false)
	if strings.Contains(storageJSON, "private-s3-value") || !strings.Contains(storageJSON, `"secret_configured":true`) {
		t.Fatal("backup storage settings should indicate the presence of the secret without returning it")
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
