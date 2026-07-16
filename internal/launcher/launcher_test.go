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

func TestUpdateDiscoversPreviousDigestOnFirstSwap(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]string{
		"docker inspect --format {{.Image}} deploybot":                           "sha256:image-id\n",
		"docker image inspect --format {{index .RepoDigests 0}} sha256:image-id": "ghcr.io/acme/deploybot@sha256:old\n",
	}}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner}
	writeTestConfig(t, l, testRunSpec())

	if err := l.Update(context.Background(), "sha256:new"); err != nil {
		t.Fatal(err)
	}
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if spec.CurrentImageDigest != "sha256:new" || spec.PreviousImageDigest != "sha256:old" {
		t.Fatalf("digest state = current %q, previous %q", spec.CurrentImageDigest, spec.PreviousImageDigest)
	}
}

func TestUpPreservesExplicitMigrationKeys(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	sessionKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	hash, err := bcrypt.GenerateFromPassword([]byte("admin-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	l := &Launcher{
		ConfigDir: t.TempDir(),
		Runner:    &fakeRunner{},
		Random:    strings.NewReader(""), // supplied keys must avoid random generation
		PromptPassword: func() (string, error) {
			t.Fatal("a supplied password hash must avoid prompting")
			return "", nil
		},
	}
	if err := l.Up(context.Background(), UpOptions{
		Image:             "ghcr.io/acme/deploybot:latest",
		EncryptionKey:     key,
		SessionKey:        sessionKey,
		AdminPasswordHash: string(hash),
	}); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}
	values := dotenvValues(t, string(env))
	if values["DEPLOYBOT_KEY"] != key || values["DEPLOYBOT_SESSION_KEY"] != sessionKey || values["DEPLOYBOT_ADMIN_HASH"] != string(hash) {
		t.Fatalf("migration values were not preserved: %+v", values)
	}
}

func TestUpSupportsAndPersistsReverseProxyConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	sessionKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner, Random: strings.NewReader("")}
	if err := l.Up(context.Background(), UpOptions{
		Image:             "ghcr.io/acme/deploybot:latest",
		NoPort:            true,
		Network:           "proxy",
		Environment:       []string{"VIRTUAL_HOST=deploy.example.com", "VIRTUAL_PORT=8080"},
		EncryptionKey:     key,
		SessionKey:        sessionKey,
		AdminPasswordHash: "already-hashed",
	}); err != nil {
		t.Fatal(err)
	}
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Ports) != 0 || spec.Network != "proxy" {
		t.Fatalf("proxy run spec = %+v", spec)
	}
	assertContainsArgs(t, runner.calls[1], "--network")
	assertContainsArgs(t, runner.calls[1], "proxy")
	for _, arg := range runner.calls[1] {
		if arg == "-p" {
			t.Fatalf("reverse-proxy deployment must not publish a host port: %v", runner.calls[1])
		}
	}
	env, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}
	values := dotenvValues(t, string(env))
	if values["VIRTUAL_HOST"] != "deploy.example.com" || values["VIRTUAL_PORT"] != "8080" {
		t.Fatalf("reverse-proxy environment = %+v", values)
	}
}

func TestUpRepairsExistingConfigurationWithoutRegeneratingSecrets(t *testing.T) {
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner}
	spec := testRunSpec()
	writeTestConfig(t, l, spec)
	adminHash, err := bcrypt.GenerateFromPassword([]byte("admin-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.envPath(), []byte("DEPLOYBOT_KEY=test\nDEPLOYBOT_ADMIN_HASH="+string(adminHash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Up(context.Background(), UpOptions{
		NoPort:      true,
		Network:     "proxy",
		Environment: []string{"VIRTUAL_HOST=deploy.example.com", "VIRTUAL_PORT=8080"},
	}); err != nil {
		t.Fatal(err)
	}
	afterSpec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSpec.Ports) != 0 || afterSpec.Network != "proxy" {
		t.Fatalf("updated run spec = %+v", afterSpec)
	}
	env, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}
	values := dotenvValues(t, string(env))
	if values["DEPLOYBOT_KEY"] != "test" || values["DEPLOYBOT_ADMIN_HASH"] != string(adminHash) || values["VIRTUAL_HOST"] != "deploy.example.com" || values["VIRTUAL_PORT"] != "8080" {
		t.Fatalf("updated environment = %+v", values)
	}
	if string(before) == string(env) {
		t.Fatal("expected reverse-proxy settings to be added")
	}
}

func TestUpBootstrapsWithExtraVolumes(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	sessionKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner, Random: strings.NewReader("")}
	mount := "/home/me/.docker/config.json:/root/.docker/config.json:ro"
	if err := l.Up(context.Background(), UpOptions{
		Image:             "ghcr.io/acme/deploybot:latest",
		Volumes:           []string{mount},
		EncryptionKey:     key,
		SessionKey:        sessionKey,
		AdminPasswordHash: "already-hashed",
	}); err != nil {
		t.Fatal(err)
	}
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(spec.Volumes, mount) {
		t.Fatalf("extra volume not persisted: %+v", spec.Volumes)
	}
	if !contains(spec.Volumes, "/var/run/docker.sock:/var/run/docker.sock") ||
		!contains(spec.Volumes, "deploybot-config:/config") ||
		!contains(spec.Volumes, "deploybot-data:/data") {
		t.Fatalf("mandatory mounts dropped: %+v", spec.Volumes)
	}
	assertContainsArgs(t, runner.calls[1], mount)
}

func TestUpAddsExtraVolumeToExistingConfig(t *testing.T) {
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner}
	writeTestConfig(t, l, testRunSpec())
	mount := "/etc/foo:/root/foo:ro"
	if err := l.Up(context.Background(), UpOptions{Volumes: []string{mount}}); err != nil {
		t.Fatal(err)
	}
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(spec.Volumes, mount) {
		t.Fatalf("extra volume not added to existing config: %+v", spec.Volumes)
	}
	if !contains(spec.Volumes, "deploybot-config:/config") {
		t.Fatalf("mandatory config mount dropped: %+v", spec.Volumes)
	}
	assertContainsArgs(t, runner.calls[1], mount)
}

func TestUpDeduplicatesRepeatedVolumeOverride(t *testing.T) {
	l := &Launcher{ConfigDir: t.TempDir(), Runner: &fakeRunner{}}
	writeTestConfig(t, l, testRunSpec())
	mount := "/etc/foo:/root/foo:ro"
	for i := 0; i < 2; i++ {
		if err := l.Up(context.Background(), UpOptions{Volumes: []string{mount}}); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, v := range spec.Volumes {
		if v == mount {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("volume %q appears %d times, want 1: %+v", mount, count, spec.Volumes)
	}
}

func TestUpdatePreservesExtraVolumes(t *testing.T) {
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner}
	spec := testRunSpec()
	mount := "/etc/foo:/root/foo:ro"
	spec.Volumes = append(spec.Volumes, mount)
	spec.CurrentImageDigest = "sha256:old"
	writeTestConfig(t, l, spec)
	if err := l.Update(context.Background(), "sha256:new"); err != nil {
		t.Fatal(err)
	}
	assertContainsArgs(t, runner.calls[3], mount)
}

func TestUpRejectsInvalidVolume(t *testing.T) {
	l := &Launcher{ConfigDir: t.TempDir(), Runner: &fakeRunner{}, Random: strings.NewReader("")}
	err := l.Up(context.Background(), UpOptions{
		Image:             "ghcr.io/acme/deploybot:latest",
		Volumes:           []string{"  "},
		AdminPasswordHash: "already-hashed",
	})
	if err == nil || !strings.Contains(err.Error(), "volume") {
		t.Fatalf("Up error = %v, want invalid volume error", err)
	}
	if _, err := os.Stat(l.runSpecPath()); !os.IsNotExist(err) {
		t.Fatalf("run spec must not be written after rejected volume: %v", err)
	}
}

func TestUpRejectsPortAndNoPortTogether(t *testing.T) {
	l := &Launcher{ConfigDir: t.TempDir(), Runner: &fakeRunner{}}
	err := l.Up(context.Background(), UpOptions{
		Image:  "ghcr.io/acme/deploybot:latest",
		Ports:  []string{"8080:8080"},
		NoPort: true,
	})
	if err == nil || !strings.Contains(err.Error(), "--no-port cannot be combined with --port") {
		t.Fatalf("Up error = %v, want conflicting port options error", err)
	}
	if _, err := os.Stat(l.runSpecPath()); !os.IsNotExist(err) {
		t.Fatalf("run spec must not be written after rejected options: %v", err)
	}
}

func TestUpdateKeepsReverseProxyConfiguration(t *testing.T) {
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner}
	spec := testRunSpec()
	spec.Ports = nil
	spec.Network = "proxy"
	spec.CurrentImageDigest = "sha256:old"
	writeTestConfig(t, l, spec)
	if err := os.WriteFile(l.envPath(), []byte("DEPLOYBOT_KEY=test\nVIRTUAL_HOST=deploy.example.com\nVIRTUAL_PORT=8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Update(context.Background(), "sha256:new"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("docker calls = %v", runner.calls)
	}
	run := runner.calls[3]
	assertContainsArgs(t, run, "--network")
	assertContainsArgs(t, run, "proxy")
	for _, arg := range run {
		if arg == "-p" {
			t.Fatalf("self-update must preserve no host-port publishing: %v", run)
		}
	}
	after, err := os.ReadFile(l.envPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("self-update must not modify the persisted proxy environment")
	}
	values := dotenvValues(t, string(after))
	if values["VIRTUAL_HOST"] != "deploy.example.com" || values["VIRTUAL_PORT"] != "8080" {
		t.Fatalf("self-update proxy environment = %+v", values)
	}
}

func testRunSpec() RunSpec {
	return RunSpec{
		Image:         "ghcr.io/acme/deploybot:latest",
		ContainerName: "deploybot",
		Ports:         []string{"8080:8080"},
		Volumes:       []string{"/var/run/docker.sock:/var/run/docker.sock", "deploybot-data:/data", "deploybot-config:/config"},
		Labels:        map[string]string{"deploybot.service": "deploybot"},
		Restart:       "unless-stopped",
		ConfigVolume:  "deploybot-config",
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
	values, err := parseEnvironment(content)
	if err != nil {
		t.Fatalf("parse dotenv: %v", err)
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
