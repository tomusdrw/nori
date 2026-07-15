package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"deploybot/internal/envfile"
	"deploybot/internal/store"
)

type LatestDigestFunc func(ctx context.Context, image string) (string, error)

type CommandRunner interface {
	Run(ctx context.Context, script string, env []string, stdout, stderr io.Writer) error
}

type OSRunner struct{}

// NormalizeNewlines converts Windows (CRLF) and classic-Mac (CR) line
// endings to Unix (LF). Browsers submit <textarea> content with CRLF
// newlines, and Bash rejects a stray carriage return inside compound
// commands (e.g. "do\r"), so scripts must be normalized before Bash
// parses or runs them.
func NormalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// ValidateScript asks Bash to parse the script without executing it.
func ValidateScript(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "bash", "-n")
	cmd.Stdin = strings.NewReader(NormalizeNewlines(script))
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("invalid bash syntax: %s", message)
}

func (OSRunner) Run(ctx context.Context, script string, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", NormalizeNewlines(script))
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type Executor struct {
	store    *store.Store
	runner   CommandRunner
	latest   LatestDigestFunc
	cooldown time.Duration

	locks    sync.Map // int64 -> *sync.Mutex
	failures sync.Map // int64 -> failureRecord
}

type failureRecord struct {
	digest string
	until  time.Time
}

func New(st *store.Store, runner CommandRunner, latest LatestDigestFunc, cooldown time.Duration) *Executor {
	if cooldown == 0 {
		cooldown = 15 * time.Minute
	}
	return &Executor{store: st, runner: runner, latest: latest, cooldown: cooldown}
}

func (e *Executor) Deploy(ctx context.Context, serviceID int64, trigger string) (int64, error) {
	mu := e.lockFor(serviceID)
	mu.Lock()
	defer mu.Unlock()

	svc, err := e.store.GetService(ctx, serviceID)
	if err != nil {
		return 0, err
	}
	if err := ValidateScript(ctx, svc.DeployScript); err != nil {
		return 0, err
	}
	digest, err := e.latest(ctx, svc.WatchedImage)
	if err != nil {
		return 0, fmt.Errorf("latest digest: %w", err)
	}
	env, err := e.buildEnv(ctx, svc, digest)
	if err != nil {
		return 0, fmt.Errorf("invalid environment file: %w", err)
	}

	if trigger == store.TriggerAuto && e.inCooldown(serviceID, digest) {
		return 0, fmt.Errorf("service %q in failure cooldown for digest %s", svc.Name, digest)
	}

	deploy := &store.Deployment{
		ServiceID:    serviceID,
		Trigger:      trigger,
		TargetDigest: digest,
		Status:       store.DeployRunning,
	}
	if err := e.store.CreateDeployment(ctx, deploy); err != nil {
		return 0, err
	}

	go e.runDeploy(svc, deploy, env)
	return deploy.ID, nil
}

func (e *Executor) runDeploy(svc *store.Service, deploy *store.Deployment, env []string) {
	ctx := context.Background()

	var logBuf bytes.Buffer
	err := e.runner.Run(ctx, svc.DeployScript, env, &logBuf, &logBuf)
	deploy.Log = logBuf.String()

	if err != nil {
		e.recordFailure(svc.ID, deploy.TargetDigest)
		e.finish(ctx, deploy, store.DeployFailed, "")
		return
	}

	e.clearFailure(svc.ID)
	e.finish(ctx, deploy, store.DeploySuccess, "")
}

func (e *Executor) buildEnv(ctx context.Context, svc *store.Service, digest string) ([]string, error) {
	content, err := e.store.GetEnvFile(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	fileEnv, err := envfile.Parse(content)
	if err != nil {
		return nil, err
	}
	env := []string{
		fmt.Sprintf("SERVICE=%s", svc.Name),
		fmt.Sprintf("TARGET_DIGEST=%s", digest),
	}
	env = append(env, fileEnv...)
	return env, nil
}

func (e *Executor) finish(ctx context.Context, d *store.Deployment, status string, extra string) {
	d.Status = status
	if extra != "" {
		d.Log += extra
	}
	now := time.Now().UTC()
	d.FinishedAt = &now
	_ = e.store.UpdateDeployment(ctx, d)
}

func (e *Executor) lockFor(id int64) *sync.Mutex {
	v, _ := e.locks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (e *Executor) inCooldown(serviceID int64, digest string) bool {
	v, ok := e.failures.Load(serviceID)
	if !ok {
		return false
	}
	rec := v.(failureRecord)
	return rec.digest == digest && time.Now().Before(rec.until)
}

func (e *Executor) recordFailure(serviceID int64, digest string) {
	e.failures.Store(serviceID, failureRecord{digest: digest, until: time.Now().Add(e.cooldown)})
}

func (e *Executor) clearFailure(serviceID int64) {
	e.failures.Delete(serviceID)
}
