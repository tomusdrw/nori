package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"deploybot/internal/docker"
	"deploybot/internal/mcpauth"
	"deploybot/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPServiceLifecycleAndScopes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mcp.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dk := &docker.Fake{Containers: map[string][]docker.Container{"app": {{ID: "c1", State: "running"}}}}
	s := &Server{store: st, docker: dk}
	handler := s.newMCPHandler()
	var scopes atomic.Value
	scopes.Store("nori:read nori:write nori:secrets")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(mcpauth.WithIdentity(r.Context(), mcpauth.Identity{ClientID: "test", Scope: scopes.Load().(string)})))
	}))
	defer srv.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 13 {
		t.Fatalf("tools=%d", len(listed.Tools))
	}
	call := func(name string, args map[string]any, wantError bool) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError != wantError {
			t.Fatalf("%s error=%t result=%+v", name, res.IsError, res)
		}
		return res
	}
	call("create_service", map[string]any{"name": "app", "watched_image": "nginx:latest", "deploy_script": "echo ok", "env_file": "SECRET=unlisted"}, false)
	svc, err := st.GetServiceByName(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"service_id": svc.ID}
	for _, name := range []string{"list_services", "get_service"} {
		a := args
		if name == "list_services" {
			a = map[string]any{}
		}
		res := call(name, a, false)
		data, _ := json.Marshal(res)
		if bytes.Contains(data, []byte("unlisted")) {
			t.Fatal("environment leaked")
		}
	}
	call("update_service", map[string]any{"service_id": svc.ID, "health_url": "https://example.com/health"}, false)
	got, _ := st.GetService(ctx, svc.ID)
	if got.DeployScript != "echo ok" || got.WatchedImage != "nginx:latest" {
		t.Fatal("partial update lost fields")
	}
	env, _ := st.GetEnvFile(ctx, svc.ID)
	if env != "SECRET=unlisted" {
		t.Fatal("partial update lost environment")
	}
	call("stop_service", args, false)
	if dk.Containers["app"][0].State != "exited" {
		t.Fatal("container not stopped")
	}
	call("start_service", args, false)
	if dk.Containers["app"][0].State != "running" {
		t.Fatal("container not started")
	}
	call("start_service", map[string]any{"service_id": 999}, true)
	call("get_container_logs", map[string]any{"service_id": svc.ID, "container_id": "another-service"}, true)
	d := &store.Deployment{ServiceID: svc.ID, Trigger: store.TriggerManual, Status: store.DeployRunning, Log: "sensitive-log-output"}
	if err := st.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"list_deployments", "get_deployment"} {
		a := args
		if name == "get_deployment" {
			a = map[string]any{"deployment_id": d.ID}
		}
		res := call(name, a, false)
		data, _ := json.Marshal(res)
		if bytes.Contains(data, []byte("sensitive-log-output")) {
			t.Fatal("logs returned without explicit request")
		}
	}
	logResult := call("get_deployment", map[string]any{"deployment_id": d.ID, "include_logs": true}, false)
	logJSON, _ := json.Marshal(logResult)
	if !bytes.Contains(logJSON, []byte("sensitive-log-output")) {
		t.Fatal("explicit logs missing")
	}

	scopes.Store("nori:read")
	call("get_service_environment", args, true)
	call("delete_service", args, true)
	scopes.Store("nori:read nori:secrets")
	res := call("get_service_environment", args, false)
	data, _ := json.Marshal(res)
	if !bytes.Contains(data, []byte("unlisted")) {
		t.Fatal("explicit environment unavailable")
	}
	scopes.Store("nori:read nori:write")
	self, err := st.EnsureSelfService(ctx, "nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"delete_service", "start_service", "stop_service", "deploy_service"} {
		call(name, map[string]any{"service_id": self.ID}, true)
	}
	call("update_service", map[string]any{"service_id": self.ID, "deploy_script": "echo unsafe"}, true)
	call("set_service_environment", map[string]any{"service_id": self.ID, "env_file": "A=B"}, true)
	call("set_service_environment", map[string]any{"service_id": svc.ID, "env_file": "NEW=value"}, false)
	call("delete_service", args, false)
	if _, err := st.GetService(ctx, svc.ID); err != store.ErrNotFound {
		t.Fatalf("not deleted: %v", err)
	}
}

func TestValidateMCPService(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*store.Service)
	}{
		{"name", func(s *store.Service) { s.Name = "../bad" }},
		{"reserved", func(s *store.Service) { s.Name = store.SelfServiceName }},
		{"image", func(s *store.Service) { s.WatchedImage = "not an image" }},
		{"policy", func(s *store.Service) { s.Policy = "unknown" }},
		{"cron", func(s *store.Service) { s.Policy = store.PolicyScheduled; s.CronExpr = "bad" }},
		{"script", func(s *store.Service) { s.DeployScript = "if" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &store.Service{Name: "app", WatchedImage: "nginx", Policy: store.PolicyManual, DeployScript: "true"}
			tc.change(svc)
			if err := validateMCPService(context.Background(), svc, nil); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
