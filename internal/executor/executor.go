package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"deploybot/internal/envfile"
	"deploybot/internal/store"
)

type LatestDigestFunc func(ctx context.Context, image string) (string, error)

// pinnedImage returns a digest-pinned reference (repo@digest) for the watched
// image. It drops any existing tag or digest from ref before appending the
// resolved digest. A registry port (the colon in "host:5000/repo") is preserved
// because it appears before the final path segment.
func pinnedImage(ref, digest string) string {
	name := ref
	if at := strings.LastIndex(name, "@"); at != -1 {
		name = name[:at]
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	return name + "@" + digest
}

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
	env, envFile, err := e.buildEnv(ctx, svc, digest)
	if err != nil {
		return 0, fmt.Errorf("invalid environment file: %w", err)
	}

	if trigger == store.TriggerAuto && e.inCooldown(serviceID, digest) {
		removeEnvFile(envFile)
		return 0, fmt.Errorf("service %q in failure cooldown for digest %s", svc.Name, digest)
	}

	deploy := &store.Deployment{
		ServiceID:    serviceID,
		Trigger:      trigger,
		TargetDigest: digest,
		Status:       store.DeployRunning,
	}
	if err := e.store.CreateDeployment(ctx, deploy); err != nil {
		removeEnvFile(envFile)
		return 0, err
	}

	log.Printf("deploy: %s %q started (id=%d digest=%s)", trigger, svc.Name, deploy.ID, shortDigest(digest))
	go e.runDeploy(svc, deploy, env, envFile)
	return deploy.ID, nil
}

func (e *Executor) runDeploy(svc *store.Service, deploy *store.Deployment, env []string, envFile string) {
	// The env file holds decrypted secrets; drop it once the script no longer
	// needs it, whichever way this deploy ends (including the self handoff).
	defer removeEnvFile(envFile)
	ctx := context.Background()

	var logBuf bytes.Buffer
	err := e.runner.Run(ctx, svc.DeployScript, env, &logBuf, &logBuf)
	deploy.Log = logBuf.String()

	if err != nil {
		log.Printf("deploy: %s %q failed (id=%d): %v", deploy.Trigger, svc.Name, deploy.ID, err)
		e.recordFailure(svc.ID, deploy.TargetDigest)
		e.finish(ctx, deploy, store.DeployFailed, "")
		return
	}

	e.clearFailure(svc.ID)
	if svc.IsSelf {
		// The detached updater has only been handed off at this point. The
		// launcher will stop this process next, so the newly booted instance
		// resolves this record from its actual running digest.
		log.Printf("deploy: %s %q handed off (id=%d)", deploy.Trigger, svc.Name, deploy.ID)
		_ = e.store.UpdateDeployment(ctx, deploy)
		return
	}
	log.Printf("deploy: %s %q succeeded (id=%d)", deploy.Trigger, svc.Name, deploy.ID)
	e.finish(ctx, deploy, store.DeploySuccess, "")
}

func shortDigest(digest string) string {
	const prefix = "sha256:"
	if strings.HasPrefix(digest, prefix) && len(digest) > len(prefix)+12 {
		return digest[:len(prefix)+12]
	}
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

// buildEnv assembles the script environment and materializes the service's
// resolved env vars as a docker --env-file (referenced by $ENV_FILE). The
// caller owns the returned path and must remove it once the deploy finishes.
func (e *Executor) buildEnv(ctx context.Context, svc *store.Service, digest string) ([]string, string, error) {
	content, err := e.store.GetEnvFile(ctx, svc.ID)
	if err != nil {
		return nil, "", err
	}
	fileEnv, err := envfile.Parse(content)
	if err != nil {
		return nil, "", err
	}
	env := []string{
		fmt.Sprintf("SERVICE=%s", svc.Name),
		fmt.Sprintf("IMAGE=%s", svc.WatchedImage),
		fmt.Sprintf("TARGET_DIGEST=%s", digest),
		fmt.Sprintf("TARGET_IMAGE=%s", pinnedImage(svc.WatchedImage, digest)),
	}
	if svc.IsSelf {
		for _, key := range []string{"DEPLOYBOT_CONFIG_VOLUME", "DEPLOYBOT_SELF_IMAGE"} {
			value := os.Getenv(key)
			if value == "" {
				return nil, "", fmt.Errorf("self-update requires %s", key)
			}
			env = append(env, key+"="+value)
		}
	}
	// Materialize last, after all validation, so failed builds leave no file.
	envFile, err := writeEnvFile(fileEnv)
	if err != nil {
		return nil, "", err
	}
	env = append(env, "ENV_FILE="+envFile)
	env = append(env, fileEnv...)
	return env, envFile, nil
}

// writeEnvFile writes the resolved "KEY=VALUE" pairs to a private temp file in
// docker --env-file format. It always creates a file, even when there are no
// vars, so $ENV_FILE is a usable path on every deploy.
func writeEnvFile(vars []string) (string, error) {
	f, err := os.CreateTemp("", "deploybot-env-*.env")
	if err != nil {
		return "", err
	}
	defer f.Close()
	var b strings.Builder
	for _, kv := range vars {
		b.WriteString(kv)
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func removeEnvFile(path string) {
	if path != "" {
		os.Remove(path)
	}
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
