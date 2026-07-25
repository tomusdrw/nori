package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"deploybot/internal/notify"
	"deploybot/internal/store"
)

// envFileValue returns the ENV_FILE=... path from a script environment.
func envFileValue(env []string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "ENV_FILE=") {
			return strings.TrimPrefix(kv, "ENV_FILE=")
		}
	}
	return ""
}

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

func TestPinnedImage(t *testing.T) {
	cases := []struct {
		name   string
		ref    string
		digest string
		want   string
	}{
		{"registry and tag", "ghcr.io/you/app:latest", "sha256:abc", "ghcr.io/you/app@sha256:abc"},
		{"registry port and tag", "registry:5000/you/app:tag", "sha256:abc", "registry:5000/you/app@sha256:abc"},
		{"registry port no tag", "registry:5000/you/app", "sha256:abc", "registry:5000/you/app@sha256:abc"},
		{"already digest pinned", "ghcr.io/you/app@sha256:old", "sha256:abc", "ghcr.io/you/app@sha256:abc"},
		{"no registry with tag", "app:latest", "sha256:abc", "app@sha256:abc"},
		{"bare name", "image", "sha256:abc", "image@sha256:abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinnedImage(tc.ref, tc.digest); got != tc.want {
				t.Errorf("pinnedImage(%q, %q) = %q, want %q", tc.ref, tc.digest, got, tc.want)
			}
		})
	}
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
	env, envFile, err := ex.buildEnv(ctx, svc, "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(envFile)
	joined := strings.Join(env, "\n")
	for _, want := range []string{"SERVICE=app", "TARGET_DIGEST=sha256:abc", "PORT=8080", "GREETING=hello world"} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment missing %q: %v", want, env)
		}
	}
}

func TestBuildEnv_InjectsImageAndEnvFile(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyManual}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnvFile(ctx, svc.ID, "PORT=8080\nGREETING=\"hello world\"\n"); err != nil {
		t.Fatal(err)
	}
	ex := New(st, &fakeRunner{}, nil, 0)
	env, envFile, err := ex.buildEnv(ctx, svc, "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(envFile)

	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"SERVICE=app",
		"IMAGE=ghcr.io/me/app:latest",
		"TARGET_DIGEST=sha256:abc",
		"TARGET_IMAGE=ghcr.io/me/app@sha256:abc",
		"ENV_FILE=" + envFile,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment missing %q: %v", want, env)
		}
	}

	// The materialized file is a docker --env-file: resolved KEY=VALUE lines
	// (quotes already stripped), holding only the service env — not the
	// deploybot metadata variables.
	info, err := os.Stat(envFile)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "PORT=8080") || !strings.Contains(got, "GREETING=hello world") {
		t.Errorf("env file content = %q", got)
	}
	if strings.Contains(got, "SERVICE=") || strings.Contains(got, "TARGET_DIGEST=") {
		t.Errorf("env file must contain only the service env, got %q", got)
	}
}

func TestDeploy_RemovesEnvFileAfterRun(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "ghcr.io/me/app:latest", Policy: store.PolicyManual, DeployScript: "echo hi"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}

	type capture struct {
		path             string
		existedDuringRun bool
	}
	seen := make(chan capture, 1)
	runner := runnerFunc(func(_ context.Context, _ string, env []string, stdout, _ io.Writer) error {
		io.WriteString(stdout, "ok\n")
		path := envFileValue(env)
		_, err := os.Stat(path)
		seen <- capture{path: path, existedDuringRun: err == nil}
		return nil
	})
	ex := New(st, runner, func(context.Context, string) (string, error) { return "sha256:new", nil }, 0)
	if _, err := ex.Deploy(ctx, svc.ID, store.TriggerManual); err != nil {
		t.Fatal(err)
	}

	var cap capture
	select {
	case cap = <-seen:
	case <-time.After(time.Second):
		t.Fatal("deploy did not run")
	}
	if cap.path == "" {
		t.Fatal("ENV_FILE was not injected into the script environment")
	}
	if !cap.existedDuringRun {
		t.Fatal("env file must exist while the deploy script runs")
	}
	// After the deploy finishes, the decrypted env file must be removed so
	// secrets do not linger on disk.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cap.path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("env file %s was not removed after the deploy", cap.path)
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

// capturingNotifier records every service-down event it receives.
type capturingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (c *capturingNotifier) NotifyServiceDown(_ context.Context, evt notify.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
	return nil
}

func (c *capturingNotifier) NotifyServiceRecovered(_ context.Context, evt notify.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
	return nil
}

func (c *capturingNotifier) snapshot() []notify.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]notify.Event, len(c.events))
	copy(out, c.events)
	return out
}

func TestExecutor_FailureFiresNotifierOnce(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{
		Name:         "app",
		WatchedImage: "ghcr.io/me/app:latest",
		Policy:       store.PolicyManual,
		DeployScript: "exit 1",
	}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	cap := &capturingNotifier{}
	ex := New(st, &fakeRunner{err: errors.New("deploy boom")}, func(context.Context, string) (string, error) {
		return "sha256:bad", nil
	}, 0)
	ex.SetNotifier(cap)
	ex.SetBotName("staging")

	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, derr := st.GetDeployment(ctx, id)
		if derr != nil {
			t.Fatal(derr)
		}
		if d.Status == store.DeployFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(cap.snapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	events := cap.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d notify events, want 1: %+v", len(events), events)
	}
	evt := events[0]
	if evt.ServiceName != "app" || evt.Trigger != store.TriggerManual {
		t.Errorf("unexpected event: %+v", evt)
	}
	if evt.Digest == "" || !strings.Contains(evt.Reason, "deploy boom") {
		t.Errorf("unexpected event payload: %+v", evt)
	}
	if evt.BotName != "staging" {
		t.Errorf("BotName = %q, want staging", evt.BotName)
	}
}

func TestExecutor_SuccessDoesNotFireNotifier(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	svc := &store.Service{Name: "app", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "echo ok"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	cap := &capturingNotifier{}
	ex := New(st, &fakeRunner{log: "ok"}, func(context.Context, string) (string, error) {
		return "sha256:good", nil
	}, 0)
	ex.SetNotifier(cap)

	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, derr := st.GetDeployment(ctx, id)
		if derr != nil {
			t.Fatal(derr)
		}
		if d.Status == store.DeploySuccess {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := cap.snapshot(); len(got) != 0 {
		t.Fatalf("success must not notify; got %+v", got)
	}
}

// waitForFailure blocks until the deploy record for id reaches DeployFailed,
// giving the synchronous notifier call inside alertFailure time to land.
func waitForFailure(t *testing.T, st *store.Store, ctx context.Context, id int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, err := st.GetDeployment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status == store.DeployFailed {
			// alertFailure runs before finish(); a tiny cushion lets the
			// notifier call return before we read the capture.
			time.Sleep(20 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deploy did not finish as failed")
}

func TestExecutor_ModeNeverSuppressesAllTriggers(t *testing.T) {
	for _, trigger := range []string{store.TriggerManual, store.TriggerAuto, store.TriggerScheduled} {
		t.Run(trigger, func(t *testing.T) {
			st := openTestStore(t)
			ctx := context.Background()
			if err := st.SetSetting(ctx, store.SettingNotifyMode, string(notify.ModeNever)); err != nil {
				t.Fatal(err)
			}
			svc := &store.Service{Name: "app", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "exit 1"}
			if err := st.CreateService(ctx, svc); err != nil {
				t.Fatal(err)
			}
			cap := &capturingNotifier{}
			ex := New(st, &fakeRunner{err: errors.New("boom")},
				func(context.Context, string) (string, error) { return "sha256:x", nil }, 0)
			ex.SetNotifier(cap)

			id, err := ex.Deploy(ctx, svc.ID, trigger)
			if err != nil {
				t.Fatalf("Deploy: %v", err)
			}
			waitForFailure(t, st, ctx, id)
			if got := cap.snapshot(); len(got) != 0 {
				t.Fatalf("mode=never must suppress %s trigger; got %+v", trigger, got)
			}
		})
	}
}

func TestExecutor_ModeAutoOnlySuppressesManualOnly(t *testing.T) {
	cases := []struct {
		trigger string
		want    int
	}{
		{store.TriggerManual, 0},
		{store.TriggerAuto, 1},
		{store.TriggerScheduled, 1},
	}
	for _, tc := range cases {
		t.Run(tc.trigger, func(t *testing.T) {
			st := openTestStore(t)
			ctx := context.Background()
			if err := st.SetSetting(ctx, store.SettingNotifyMode, string(notify.ModeAutoOnly)); err != nil {
				t.Fatal(err)
			}
			svc := &store.Service{Name: "app", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "exit 1"}
			if err := st.CreateService(ctx, svc); err != nil {
				t.Fatal(err)
			}
			cap := &capturingNotifier{}
			ex := New(st, &fakeRunner{err: errors.New("boom")},
				func(context.Context, string) (string, error) { return "sha256:x", nil }, 0)
			ex.SetNotifier(cap)

			id, err := ex.Deploy(ctx, svc.ID, tc.trigger)
			if err != nil {
				t.Fatalf("Deploy: %v", err)
			}
			waitForFailure(t, st, ctx, id)
			if got := len(cap.snapshot()); got != tc.want {
				t.Fatalf("mode=auto-only trigger=%s: got %d events, want %d", tc.trigger, got, tc.want)
			}
		})
	}
}

func TestExecutor_ModeAlwaysFiresForAllTriggers(t *testing.T) {
	for _, trigger := range []string{store.TriggerManual, store.TriggerAuto, store.TriggerScheduled} {
		t.Run(trigger, func(t *testing.T) {
			st := openTestStore(t)
			ctx := context.Background()
			if err := st.SetSetting(ctx, store.SettingNotifyMode, string(notify.ModeAlways)); err != nil {
				t.Fatal(err)
			}
			svc := &store.Service{Name: "app", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "exit 1"}
			if err := st.CreateService(ctx, svc); err != nil {
				t.Fatal(err)
			}
			cap := &capturingNotifier{}
			ex := New(st, &fakeRunner{err: errors.New("boom")},
				func(context.Context, string) (string, error) { return "sha256:x", nil }, 0)
			ex.SetNotifier(cap)

			id, err := ex.Deploy(ctx, svc.ID, trigger)
			if err != nil {
				t.Fatalf("Deploy: %v", err)
			}
			waitForFailure(t, st, ctx, id)
			if got := len(cap.snapshot()); got != 1 {
				t.Fatalf("mode=always trigger=%s: got %d events, want 1", trigger, got)
			}
		})
	}
}

func TestExecutor_UnsetModeDefaultsToAlways(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	// Intentionally do NOT set SettingNotifyMode.
	svc := &store.Service{Name: "app", WatchedImage: "img", Policy: store.PolicyManual, DeployScript: "exit 1"}
	if err := st.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	cap := &capturingNotifier{}
	ex := New(st, &fakeRunner{err: errors.New("boom")},
		func(context.Context, string) (string, error) { return "sha256:x", nil }, 0)
	ex.SetNotifier(cap)

	id, err := ex.Deploy(ctx, svc.ID, store.TriggerManual)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	waitForFailure(t, st, ctx, id)
	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("unset mode must default to always: got %d events, want 1", got)
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
