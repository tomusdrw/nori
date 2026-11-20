package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"nori/internal/backup"
	"nori/internal/docker"
	"nori/internal/mcpauth"
	"nori/internal/store"
)

func TestMCPServiceBackupPolicy(t *testing.T) {
	ctx := context.Background()
	t.Setenv("NORI_BACKUP_DIR", "")
	t.Setenv("NORI_BACKUP_RECIPIENT", "")
	path := filepath.Join(t.TempDir(), "nori.db")
	st, err := store.Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dk := &docker.Fake{}
	server := &Server{store: st, docker: dk}
	handler := server.newMCPHandler()
	manager, err := backup.New(ctx, st, dk, nil, path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	server.SetBackups(manager)

	svc := &store.Service{Name: "backup-app", DeploymentMode: store.DeploymentModeCustom, Policy: store.PolicyManual}
	env := "BACKUP_TOKEN=private-backup-hook\n"
	if err := st.SaveServiceConfig(ctx, svc, &env, nil); err != nil {
		t.Fatal(err)
	}

	connect := func(scope string) *mcp.ClientSession {
		t.Helper()
		httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := mcpauth.Identity{ClientID: "test", Scope: scope}
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
	reader := connect("nori:read")
	writer := connect("nori:read nori:write")
	if !strings.Contains(writer.InitializeResult().Instructions, "get_service_backup_policy") {
		t.Error("server instructions do not guide clients to the backup tools")
	}

	call := func(session *mcp.ClientSession, name string, args map[string]any, wantError bool) string {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError != wantError {
			t.Fatalf("%s error = %t, want %t: %+v", name, result.IsError, wantError, result)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	args := map[string]any{"service_id": svc.ID}
	defaults := call(reader, "get_service_backup_policy", args, false)
	if !strings.Contains(defaults, `"keep":7`) || !strings.Contains(defaults, store.DefaultBackupPolicy(svc.ID).Schedule) {
		t.Fatalf("missing default backup policy: %s", defaults)
	}
	if err := manager.SaveS3(ctx, backup.S3Config{
		Endpoint: "https://object-store.example", Region: "us-east-1", Prefix: "nori",
		Bucket: "private-backups", AccessKeyID: "private-access-id", SecretAccessKey: "private-access-key",
	}); err != nil {
		t.Fatal(err)
	}
	remote := call(reader, "get_service_backup_policy", args, false)
	if !strings.Contains(remote, `"s3_upload_enabled":true`) || strings.Contains(remote, "private-") || strings.Contains(remote, "object-store.example") {
		t.Fatal("backup policy response should expose only the S3 upload status")
	}

	policy := store.DefaultBackupPolicy(svc.ID)
	policy.Keep = 9
	policy.LocalOnly = true
	policy.Export = "echo private-backup-hook"
	policy.Import = "true"
	if err := manager.SavePolicy(ctx, *svc, policy); err != nil {
		t.Fatal(err)
	}
	call(reader, "update_service_backup_policy", map[string]any{"service_id": svc.ID, "keep": 2}, true)
	call(writer, "update_service_backup_policy", map[string]any{"service_id": svc.ID, "schedule": "bad cron", "keep": 2}, true)
	before, _, err := st.BackupPolicy(ctx, svc.ID)
	if err != nil || before.Keep != 9 {
		t.Fatalf("rejected update changed the policy: %+v, %v", before, err)
	}

	updated := call(writer, "update_service_backup_policy", map[string]any{
		"service_id": svc.ID,
		"schedule":   "",
		"sources":    []map[string]any{{"type": "volume", "path": "app-data"}},
	}, false)
	current := call(reader, "get_service_backup_policy", args, false)
	for _, response := range []string{updated, current} {
		if strings.Contains(response, "private-backup-hook") || !strings.Contains(response, `"export":"echo [REDACTED]"`) ||
			!strings.Contains(response, `"import":"true"`) || !strings.Contains(response, `"path":"app-data"`) {
			t.Fatalf("unexpected backup policy response: %s", response)
		}
	}
	saved, _, err := st.BackupPolicy(ctx, svc.ID)
	if err != nil || saved.Schedule != "" || saved.Keep != 9 || !saved.LocalOnly || saved.Export != policy.Export || saved.Import != policy.Import {
		t.Fatalf("partial update did not preserve existing settings: %+v, %v", saved, err)
	}

	call(writer, "update_service_backup_policy", map[string]any{
		"service_id": svc.ID, "local_only": false, "sources": []any{},
	}, false)
	cleared, _, err := st.BackupPolicy(ctx, svc.ID)
	if err != nil || cleared.LocalOnly || len(cleared.Sources) != 0 {
		t.Fatalf("explicit false and empty list should clear settings: %+v, %v", cleared, err)
	}

	hooks := map[string]any{
		"service_id": svc.ID, "quiesce": "echo quiesce", "resume": "echo resume",
		"export": "echo export", "import": "echo import",
	}
	call(reader, "update_service_backup_policy", hooks, true)
	call(writer, "update_service_backup_policy", map[string]any{"service_id": svc.ID, "quiesce": "true"}, true)
	call(writer, "update_service_backup_policy", map[string]any{"service_id": svc.ID, "export": "if"}, true)
	updatedHooks := call(writer, "update_service_backup_policy", hooks, false)
	for _, response := range []string{updatedHooks, call(reader, "get_service_backup_policy", args, false)} {
		for _, key := range []string{"quiesce", "resume", "export", "import"} {
			if !strings.Contains(response, fmt.Sprintf(`"%s":"echo %s"`, key, key)) {
				t.Fatalf("missing configured %s command: %s", key, response)
			}
		}
	}
	call(writer, "update_service_backup_policy", map[string]any{"service_id": svc.ID, "export": ""}, true)
	call(writer, "update_service_backup_policy", map[string]any{"service_id": svc.ID, "export": "", "import": ""}, false)
	hookPolicy, _, err := st.BackupPolicy(ctx, svc.ID)
	if err != nil || hookPolicy.Export != "" || hookPolicy.Import != "" ||
		hookPolicy.Quiesce != "echo quiesce" || hookPolicy.Resume != "echo resume" {
		t.Fatalf("clearing export/import must preserve quiesce/resume: %+v, %v", hookPolicy, err)
	}

	self, err := st.EnsureSelfService(ctx, "nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	selfArgs := map[string]any{"service_id": self.ID}
	call(reader, "get_service_backup_policy", selfArgs, true)
	call(writer, "update_service_backup_policy", selfArgs, true)

	call(reader, "create_service_backup", args, true)
	queued := call(writer, "create_service_backup", args, false)
	if !strings.Contains(queued, `"backup_id":`) {
		t.Fatalf("missing queued backup ID: %s", queued)
	}
	call(writer, "create_service_backup", args, true)
	history := call(reader, "list_service_backups", args, false)
	if !strings.Contains(history, `"status":"queued"`) {
		t.Fatalf("missing queued backup in history: %s", history)
	}
	jobs, err := st.BackupHistory(ctx, svc.ID)
	if err != nil || len(jobs) != 1 || !strings.Contains(queued, fmt.Sprintf(`"backup_id":%d`, jobs[0].ID)) {
		t.Fatalf("queued backup ID did not match history: %v, %v", jobs, err)
	}
	jobArgs := map[string]any{"service_id": svc.ID, "backup_id": jobs[0].ID}
	call(reader, "retry_service_backup_upload", jobArgs, true)
	call(writer, "retry_service_backup_upload", jobArgs, true)
}
