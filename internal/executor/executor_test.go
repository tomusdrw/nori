package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"deploybot/internal/store"
)

// crlfScript is a valid Bash script carrying Windows (CRLF) line endings,
// as a browser submits a <textarea>. Bash rejects the stray carriage
// return before "do" unless newlines are normalized first.
const crlfScript = "echo start\r\nfor i in 1 2 3; do\r\n  echo \"line $i\"\r\ndone\r\n"

func TestValidateScript_AcceptsCRLFLineEndings(t *testing.T) {
	if err := ValidateScript(context.Background(), crlfScript); err != nil {
		t.Fatalf("CRLF script must validate after normalization: %v", err)
	}
}

func TestOSRunner_RunsCRLFScript(t *testing.T) {
	var out bytes.Buffer
	if err := (OSRunner{}).Run(context.Background(), crlfScript, nil, &out, &out); err != nil {
		t.Fatalf("Run CRLF script: %v (output=%q)", err, out.String())
	}
	if !strings.Contains(out.String(), "line 2") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

type fakeRunner struct {
	err    error
	log    string
	called bool
}

type runnerFunc func(ctx context.Context, script string, env []string, stdout, stderr io.Writer) error

func (f runnerFunc) Run(ctx context.Context, script string, env []string, stdout, stderr io.Writer) error {
	return f(ctx, script, env, stdout, stderr)
}

func (f *fakeRunner) Run(ctx context.Context, script string, env []string, stdout, stderr io.Writer) error {
	f.called = true
	if f.log != "" {
		io.WriteString(stdout, f.log)
	}
	return f.err
}

func TestDeploy_InvalidBashIsRejectedBeforeRun(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyManual, DeployScript: "if true; then\n  echo broken"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	ex := New(st, runner, func(context.Context, string) (string, error) {
		return "sha256:new", nil
	}, 0)
	if _, err := ex.Deploy(ctx, svc.ID, store.TriggerManual); err == nil {
		t.Fatal("expected invalid Bash to be rejected")
	}
	if runner.called {
		t.Fatal("invalid Bash must not be executed")
	}
	assertNoDeployments(t, st, svc.ID)
}

func TestDeploy_InvalidEnvFileIsRejectedBeforeRun(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnvFile(ctx, svc.ID, "NOT VALID"); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	ex := New(st, runner, func(context.Context, string) (string, error) {
		return "sha256:new", nil
	}, 0)
	if _, err := ex.Deploy(ctx, svc.ID, store.TriggerManual); err == nil {
		t.Fatal("expected invalid environment file to be rejected")
	}
	if runner.called {
		t.Fatal("a deployment with an invalid environment must not run")
	}
	assertNoDeployments(t, st, svc.ID)
}

func TestBuildEnv_UsesCompleteDotenvFile(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "image", Policy: store.PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnvFile(ctx, svc.ID, "PORT=8080\nGREETING=\"hello world\"\n"); err != nil {
		t.Fatal(err)
	}
	ex := New(st, &fakeRunner{}, nil, 0)
	env, err := ex.buildEnv(ctx, svc, "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"SERVICE=app", "TARGET_DIGEST=sha256:abc", "PORT=8080", "GREETING=hello world"} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment missing %q: %v", want, env)
		}
	}
}

func TestDeploy_Success(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyManual, DeployScript: "echo hi"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{log: "deployed\n"}
	ex := New(st, runner, func(ctx context.Context, image string) (string, error) {
		return "sha256:new", nil
	}, 0)

	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// wait for async deploy
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, err := st.GetDeployment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != store.DeployRunning {
			if d.Status != store.DeploySuccess {
				t.Fatalf("status = %s", d.Status)
			}
			if !strings.Contains(d.Log, "deployed") {
				t.Fatalf("log = %q", d.Log)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deploy did not finish")
}

func TestDeploy_FailureCooldown(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyImmediate, DeployScript: "exit 1"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{err: errors.New("boom")}
	latest := func(ctx context.Context, image string) (string, error) { return "sha256:bad", nil }
	ex := New(st, runner, latest, time.Minute)

	if id, err := ex.Deploy(ctx, svc.ID, store.TriggerAuto); err != nil || id == 0 {
		t.Fatalf("expected deploy to start, id=%d err=%v", id, err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := ex.Deploy(ctx, svc.ID, store.TriggerAuto); err == nil {
		t.Fatal("expected cooldown block")
	}
}

func TestDeploy_SelfHandoffStaysRunningAndGetsLauncherEnv(t *testing.T) {
	t.Setenv("DEPLOYBOT_CONFIG_VOLUME", "deploybot-config")
	t.Setenv("DEPLOYBOT_SELF_IMAGE", "ghcr.io/acme/deploybot:latest")
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{
		Name:         store.SelfServiceName,
		WatchedImage: "ghcr.io/acme/deploybot:latest",
		Policy:       store.PolicyManual,
		DeployScript: store.SelfDeployScript,
		IsSelf:       true,
	}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	runner := runnerFunc(func(_ context.Context, _ string, env []string, stdout, _ io.Writer) error {
		io.WriteString(stdout, "updater launched\n")
		done <- env
		return nil
	})
	ex := New(st, runner, func(context.Context, string) (string, error) {
		return "sha256:new", nil
	}, 0)
	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	select {
	case env = <-done:
	case <-time.After(time.Second):
		t.Fatal("self handoff was not executed")
	}
	// The runner has returned; give the asynchronous executor a moment to
	// persist its intentionally-running handoff record before reading SQLite.
	time.Sleep(20 * time.Millisecond)
	d, err := st.GetDeployment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != store.DeployRunning || d.FinishedAt != nil {
		t.Fatalf("self deployment was finalized before replacement: %+v", d)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"DEPLOYBOT_CONFIG_VOLUME=deploybot-config", "DEPLOYBOT_SELF_IMAGE=ghcr.io/acme/deploybot:latest"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from handoff environment: %v", want, env)
		}
	}
}

func TestDeploy_SelfHandoffFailureIsFinalized(t *testing.T) {
	t.Setenv("DEPLOYBOT_CONFIG_VOLUME", "deploybot-config")
	t.Setenv("DEPLOYBOT_SELF_IMAGE", "image")
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: store.SelfServiceName, WatchedImage: "image", Policy: store.PolicyManual, DeployScript: "exit 1", IsSelf: true}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	ex := New(st, runnerFunc(func(_ context.Context, _ string, _ []string, _ io.Writer, _ io.Writer) error {
		done <- struct{}{}
		return errors.New("handoff failed")
	}), func(context.Context, string) (string, error) { return "sha256:new", nil }, 0)
	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("self handoff did not execute")
	}
	time.Sleep(20 * time.Millisecond)
	d, err := st.GetDeployment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != store.DeployFailed || d.FinishedAt == nil {
		t.Fatalf("failed self handoff was not finalized: %+v", d)
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func assertNoDeployments(t *testing.T, st *store.Store, serviceID int64) {
	t.Helper()
	deployments, err := st.ListDeployments(context.Background(), serviceID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 0 {
		t.Fatalf("invalid configuration created %d deployment records", len(deployments))
	}
}
