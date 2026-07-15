package launcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type fakeRunner struct {
	calls   [][]string
	outputs map[string]string
	err     error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.err
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.outputs[strings.Join(append([]string{name}, args...), " ")], nil
}

func TestUpBootstrapsOnceAndReusesExistingSecrets(t *testing.T) {
	runner := &fakeRunner{}
	prompts := 0
	l := &Launcher{
		ConfigDir: t.TempDir(),
		Runner:    runner,
		Random:    bytes.NewReader(bytes.Repeat([]byte{7}, 64)),
		PromptPassword: func() (string, error) {
			prompts++
			return "admin-password", nil
		},
	}
	opts := UpOptions{Image: "ghcr.io/acme/deploybot:latest"}
	if err := l.Up(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Fatalf("prompt count = %d, want 1", prompts)
	}
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if spec.ConfigVolume != DefaultConfigVolume || spec.ContainerName != DefaultContainer {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	env, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}
	values := dotenvValues(t, string(env))
	key, err := base64.StdEncoding.DecodeString(values["DEPLOYBOT_KEY"])
	if err != nil || len(key) != 32 {
		t.Fatalf("DEPLOYBOT_KEY = %q, decoded=%d, err=%v", values["DEPLOYBOT_KEY"], len(key), err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(values["DEPLOYBOT_ADMIN_HASH"]), []byte("admin-password")); err != nil {
		t.Fatalf("admin hash did not match prompted password: %v", err)
	}
	before := string(env)

	// An existing config must neither prompt nor consume a new random source.
	l.PromptPassword = func() (string, error) {
		t.Fatal("existing config must not prompt")
		return "", nil
	}
	l.Random = strings.NewReader("")
	if err := l.Up(context.Background(), UpOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatal("existing launcher environment was regenerated")
	}
	if len(runner.calls) != 4 { // rm/run for first boot, then rm/run again
		t.Fatalf("docker calls = %v", runner.calls)
	}
	assertContainsArgs(t, runner.calls[1], "DEPLOYBOT_SELF_IMAGE=ghcr.io/acme/deploybot:latest")
	assertContainsArgs(t, runner.calls[1], "deploybot-config:/config")
}

func TestUpdateSwapsToDigestAndRecordsPrevious(t *testing.T) {
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner}
	writeTestConfig(t, l, RunSpec{
		Image:              "ghcr.io/acme/deploybot:latest",
		ContainerName:      "deploybot",
		Ports:              []string{"8080:8080"},
		Volumes:            []string{"/var/run/docker.sock:/var/run/docker.sock", "deploybot-data:/data", "deploybot-config:/config"},
		Labels:             map[string]string{"deploybot.service": "deploybot"},
		Restart:            "unless-stopped",
		ConfigVolume:       "deploybot-config",
		CurrentImageDigest: "sha256:old",
	})
	if err := l.Update(context.Background(), "sha256:new"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %v", runner.calls)
	}
	wantPrefixes := [][]string{
		{"docker", "pull", "ghcr.io/acme/deploybot:latest@sha256:new"},
		{"docker", "stop", "deploybot"},
		{"docker", "rm", "deploybot"},
		{"docker", "run", "-d", "--name", "deploybot"},
	}
	for i, want := range wantPrefixes {
		if got := runner.calls[i]; len(got) < len(want) || !equal(got[:len(want)], want) {
			t.Fatalf("call %d = %v, want prefix %v", i, got, want)
		}
	}
	assertContainsArgs(t, runner.calls[3], "ghcr.io/acme/deploybot:latest@sha256:new")
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if spec.CurrentImageDigest != "sha256:new" || spec.PreviousImageDigest != "sha256:old" {
		t.Fatalf("digest state = current %q, previous %q", spec.CurrentImageDigest, spec.PreviousImageDigest)
	}
}

func writeTestConfig(t *testing.T, l *Launcher, spec RunSpec) {
	t.Helper()
	if err := os.WriteFile(l.envPath(), []byte("DEPLOYBOT_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.writeSpec(spec); err != nil {
		t.Fatal(err)
	}
}

func dotenvValues(t *testing.T, content string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid dotenv line %q", line)
		}
		values[parts[0]] = parts[1]
	}
	return values
}

func assertContainsArgs(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Fatalf("args %v do not contain %q", args, want)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
