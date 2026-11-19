package web

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"nori/internal/executor"
	"nori/internal/store"
)

type quietDeployRunner struct{ finish <-chan struct{} }

func (r quietDeployRunner) Run(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
	<-r.finish
	_, err := io.WriteString(stdout, "finished <script>")
	return err
}

func TestDeploymentStreamTracksQuietRunAndCompletion(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "nori.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := &store.Service{
		Name: "app", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript,
		DeployScript: "true",
	}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	stale := &store.Deployment{ServiceID: svc.ID, Status: store.DeployRunning, Trigger: store.TriggerManual}
	if err := st.CreateDeployment(ctx, stale); err != nil {
		t.Fatal(err)
	}

	finish := make(chan struct{})
	defer func() {
		if finish != nil {
			close(finish)
		}
	}()
	ex := executor.New(st, quietDeployRunner{finish: finish}, nil, 0)
	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st, executor: ex}
	router := chi.NewRouter()
	router.Get("/deployments/{id}/stream", srv.handleDeploymentStream)
	stream := func(id int64) string {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/deployments/%d/stream", id), nil))
		if rec.Code != 200 || rec.Header().Get("X-Nori-Log-Refresh") != "ok" {
			t.Fatalf("deployment refresh: status %d, headers %v, body %q", rec.Code, rec.Header(), rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := stream(id); !strings.Contains(body, "Output will appear when the run finishes.") ||
		!strings.Contains(body, `hx-trigger="every 2s"`) || !strings.Contains(body, "Elapsed") {
		t.Errorf("quiet active deployment: %s", body)
	}
	if body := stream(stale.ID); !strings.Contains(body, "Nori can’t confirm this run is still active.") {
		t.Errorf("stale deployment: %s", body)
	}

	running, err := st.GetDeployment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	close(finish)
	finish = nil
	deadline := time.Now().Add(5 * time.Second)
	for {
		body := stream(id)
		if strings.Contains(body, "badge-deploy-success") && !ex.DeploymentActive(id) {
			if strings.Contains(body, "hx-trigger") || !strings.Contains(body, "finished &lt;script&gt;") {
				t.Fatalf("completed deployment: %s", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not finish: %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The worker can finish between the handler's read and its activity check.
	progress := srv.deploymentProgress(ctx, running)
	if progress.Status != store.DeploySuccess || progress.Log != "finished <script>" || progress.Elapsed == "" || progress.Poll {
		t.Errorf("progress from pre-completion read: %+v", progress)
	}
}
