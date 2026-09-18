package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deploybot/internal/auth"
	"deploybot/internal/docker"
	"deploybot/internal/executor"
	"deploybot/internal/launcher"
	"deploybot/internal/poller"
	"deploybot/internal/store"
)

var protectedLauncherEnvironment = []struct {
	key   string
	value string
}{
	{"DEPLOYBOT_KEY", "encryption-secret"},
	{"DEPLOYBOT_SESSION_KEY", "session-secret"},
	{"DEPLOYBOT_ADMIN_HASH", "admin-secret"},
	{"DEPLOYBOT_CONFIG_VOLUME", "config-volume-secret"},
	{"DEPLOYBOT_SELF_CONTAINER", "self-container-secret"},
	{"DEPLOYBOT_SELF_IMAGE", "self-image-secret"},
}

func TestSelfServiceConfigureEditsLauncherEnvironmentAndKeepsRedeployAvailable(t *testing.T) {
	_, self, srv, cookies, _ := newSelfServiceServer(t)

	configDir := t.TempDir()
	launcherEnvPath := filepath.Join(configDir, launcher.EnvFilename)
	var initial strings.Builder
	for _, protected := range protectedLauncherEnvironment {
		initial.WriteString(protected.key + "=" + protected.value + "\n")
	}
	initial.WriteString("DEPLOYBOT_POLL_INTERVAL=60s\nVIRTUAL_HOST=old.example.com\n")
	if err := os.WriteFile(launcherEnvPath, []byte(initial.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	selfEnvironment := &launcher.Launcher{ConfigDir: configDir}
	srv.selfEnvironment = selfEnvironment

	body := getAuthed(t, srv, cookies, "/services/"+self.Name+"/edit")
	for _, want := range []string{
		"DEPLOYBOT_POLL_INTERVAL=60s",
		"VIRTUAL_HOST=old.example.com",
		"Save these values, then use Re-deploy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("self configure page missing %q\n%s", want, body)
		}
	}
	for _, protected := range protectedLauncherEnvironment {
		if strings.Contains(body, protected.value) {
			t.Errorf("self configure page exposes launcher-managed value for %s", protected.key)
		}
	}
	editorStart := strings.Index(body, `data-editor="launcher-env"`)
	if editorStart < 0 {
		t.Fatalf("launcher environment editor not found")
	}
	tagStart := strings.LastIndex(body[:editorStart], "<textarea")
	if tagStart < 0 {
		t.Fatalf("launcher environment opening tag not found")
	}
	tagEnd := strings.Index(body[tagStart:], ">")
	if tagEnd < 0 {
		t.Fatalf("launcher environment opening tag is not closed")
	}
	if strings.Contains(body[tagStart:tagStart+tagEnd], "readonly") {
		t.Fatal("launcher environment editor is still read-only")
	}
	editorEnd := strings.Index(body[tagStart:], "</textarea>")
	if editorEnd < 0 {
		t.Fatal("launcher environment editor is not closed")
	}
	editor := body[tagStart : tagStart+editorEnd]
	for _, protected := range protectedLauncherEnvironment {
		if strings.Contains(editor, protected.key+"=") {
			t.Errorf("launcher environment editor exposes protected key %s", protected.key)
		}
	}

	csrf := csrfFromBody(body)
	form := url.Values{
		"csrf_token":    {csrf},
		"name":          {self.Name},
		"watched_image": {self.WatchedImage},
		"policy":        {"manual"},
		"deploy_script": {store.SelfDeployScript},
		"env_file": {
			"DEPLOYBOT_POLL_INTERVAL=15s\n" +
				"VIRTUAL_HOST=new.example.com\n",
		},
	}
	postAuthed(t, srv, cookies, "/services/"+self.Name, form.Encode())

	persisted, err := os.ReadFile(launcherEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, protected := range protectedLauncherEnvironment {
		want := protected.key + "=" + protected.value
		if !strings.Contains(string(persisted), want) {
			t.Errorf("persisted launcher environment missing %q:\n%s", want, persisted)
		}
	}
	for _, want := range []string{"DEPLOYBOT_POLL_INTERVAL=15s", "VIRTUAL_HOST=new.example.com"} {
		if !strings.Contains(string(persisted), want) {
			t.Errorf("persisted launcher environment missing %q:\n%s", want, persisted)
		}
	}
	if strings.Contains(string(persisted), "VIRTUAL_HOST=old.example.com") {
		t.Errorf("old launcher value remained after replacement:\n%s", persisted)
	}

	detail := getAuthed(t, srv, cookies, "/services/"+self.Name)
	if !strings.Contains(detail, ">Re-deploy<") {
		t.Errorf("self-service detail no longer offers re-deploy:\n%s", detail)
	}
}

func TestLauncherEnvironmentValidationRejectsProtectedKeys(t *testing.T) {
	for _, protected := range protectedLauncherEnvironment {
		t.Run(protected.key, func(t *testing.T) {
			body := url.Values{
				"kind":    {"launcher-env"},
				"content": {protected.key + "=replaced\n"},
			}.Encode()
			req := httptest.NewRequest(http.MethodPost, "/validate/editor", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rr := httptest.NewRecorder()
			(&Server{}).handleEditorValidate(rr, req)

			var result editorValidation
			if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.Valid || !strings.Contains(result.Message, "launcher-managed") {
				t.Fatalf("unexpected validation result: %+v", result)
			}
		})
	}
}

func TestSelfServiceDetailDoesNotRequireReadableLauncherEnvironment(t *testing.T) {
	_, self, srv, cookies, _ := newSelfServiceServer(t)
	srv.selfEnvironment = &fakeSelfEnvironment{readErr: errors.New("launcher environment unavailable")}

	detail := getAuthed(t, srv, cookies, "/services/"+self.Name)
	if !strings.Contains(detail, ">Re-deploy<") {
		t.Errorf("self-service detail does not offer re-deploy:\n%s", detail)
	}
}

func TestSelfServiceUpdateDoesNotWriteEnvironmentWhenServiceUpdateFails(t *testing.T) {
	st, self, srv, cookies, dbPath := newSelfServiceServer(t)
	selfEnvironment := &fakeSelfEnvironment{}
	srv.selfEnvironment = selfEnvironment
	installUpdateFailureTrigger(t, dbPath, "fail_all_service_updates", "1", "service update blocked")

	body := getAuthed(t, srv, cookies, "/services/"+self.Name+"/edit")
	rr := postSelfServiceUpdate(t, srv, cookies, self, csrfFromBody(body), "immediate", "NEW_VALUE=1\n")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "service update blocked") {
		t.Fatalf("unexpected response status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(selfEnvironment.replacements) != 0 {
		t.Fatalf("launcher environment was written despite failed service update: %q", selfEnvironment.replacements)
	}
	persisted, err := st.GetService(context.Background(), self.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Policy != store.PolicyManual {
		t.Fatalf("policy = %q after failed update, want %q", persisted.Policy, store.PolicyManual)
	}
}

func TestSelfServiceUpdateRollsBackServiceWhenEnvironmentWriteFails(t *testing.T) {
	st, self, srv, cookies, _ := newSelfServiceServer(t)
	selfEnvironment := &fakeSelfEnvironment{writeErr: errors.New("launcher environment write failed")}
	srv.selfEnvironment = selfEnvironment

	body := getAuthed(t, srv, cookies, "/services/"+self.Name+"/edit")
	rr := postSelfServiceUpdate(t, srv, cookies, self, csrfFromBody(body), "immediate", "NEW_VALUE=1\n")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "launcher environment write failed") {
		t.Fatalf("unexpected response status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(selfEnvironment.replacements) != 1 {
		t.Fatalf("launcher environment writes = %d, want 1", len(selfEnvironment.replacements))
	}
	persisted, err := st.GetService(context.Background(), self.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Policy != store.PolicyManual {
		t.Fatalf("policy = %q after launcher write failure, want rolled back to %q", persisted.Policy, store.PolicyManual)
	}
}

func TestSelfServiceUpdateReportsRollbackFailure(t *testing.T) {
	st, self, srv, cookies, dbPath := newSelfServiceServer(t)
	srv.selfEnvironment = &fakeSelfEnvironment{writeErr: errors.New("launcher environment write failed")}
	installUpdateFailureTrigger(t, dbPath, "fail_service_rollback", "OLD.policy = 'immediate' AND NEW.policy = 'manual'", "service rollback blocked")

	body := getAuthed(t, srv, cookies, "/services/"+self.Name+"/edit")
	rr := postSelfServiceUpdate(t, srv, cookies, self, csrfFromBody(body), "immediate", "NEW_VALUE=1\n")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"launcher environment write failed", "service rollback blocked"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("response missing %q:\n%s", want, rr.Body.String())
		}
	}
	persisted, err := st.GetService(context.Background(), self.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Policy != store.PolicyImmediate {
		t.Fatalf("policy = %q after failed rollback, want %q", persisted.Policy, store.PolicyImmediate)
	}
}

type fakeSelfEnvironment struct {
	content      string
	readErr      error
	writeErr     error
	replacements []string
}

func (f *fakeSelfEnvironment) EditableEnvironment() (string, error) {
	return f.content, f.readErr
}

func (f *fakeSelfEnvironment) ReplaceEditableEnvironment(content string) error {
	f.replacements = append(f.replacements, content)
	if f.writeErr == nil {
		f.content = content
	}
	return f.writeErr
}

func newSelfServiceServer(t *testing.T) (*store.Store, *store.Service, *Server, []*http.Cookie, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "self.db")
	st, err := store.Open(dbPath, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	self, err := st.EnsureSelfService(context.Background(), "ghcr.io/acme/nori:latest")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	latest := func(context.Context, string) (string, error) { return "sha256:new", nil }
	ex := executor.New(st, &executor.OSRunner{}, latest, 0)
	pl := poller.New(st, latest, ex, 0)
	srv := NewServer(st, &docker.Fake{}, ex, pl, a)
	return st, self, srv, loginCookies(t, srv), dbPath
}

func postSelfServiceUpdate(t *testing.T, srv *Server, cookies []*http.Cookie, self *store.Service, csrf, policy, env string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"csrf_token":    {csrf},
		"name":          {self.Name},
		"watched_image": {self.WatchedImage},
		"policy":        {policy},
		"deploy_script": {store.SelfDeployScript},
		"env_file":      {env},
	}
	req := httptest.NewRequest(http.MethodPost, "/services/"+self.Name, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func installUpdateFailureTrigger(t *testing.T, dbPath, name, condition, message string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statement := fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE UPDATE ON service WHEN %s BEGIN SELECT RAISE(FAIL, '%s'); END`,
		name, condition, strings.ReplaceAll(message, "'", "''"),
	)
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}
