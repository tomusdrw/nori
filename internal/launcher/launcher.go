// Package launcher owns the configuration used to create and replace the
// deploybot container. It deliberately uses the Docker CLI: the configuration
// maps one-to-one to visible docker run flags and the launcher image already
// includes that CLI.
package launcher

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"deploybot/internal/auth"
	"golang.org/x/term"
)

const (
	DefaultConfigDir    = "/config"
	RunSpecFilename     = "run.json"
	EnvFilename         = "deploybot.env"
	DefaultConfigVolume = "deploybot-config"
	DefaultDataVolume   = "deploybot-data"
	DefaultContainer    = "deploybot"
	DefaultPort         = "8080:8080"
)

// RunSpec is the launcher-owned, persistent description of the deploybot
// container. The corresponding dotenv file contains the application config
// and secrets, rather than putting them in this JSON document.
type RunSpec struct {
	Image               string            `json:"image"`
	ContainerName       string            `json:"container_name"`
	Ports               []string          `json:"ports"`
	Volumes             []string          `json:"volumes"`
	Labels              map[string]string `json:"labels"`
	Restart             string            `json:"restart"`
	ConfigVolume        string            `json:"config_volume"`
	CurrentImageDigest  string            `json:"current_image_digest,omitempty"`
	PreviousImageDigest string            `json:"previous_image_digest,omitempty"`
}

// UpOptions are consulted only when no launcher configuration exists yet.
// Once bootstrapped, the persisted RunSpec is authoritative.
type UpOptions struct {
	Image             string
	ConfigVolume      string
	ContainerName     string
	DataVolume        string
	Ports             []string
	EncryptionKey     string
	SessionKey        string
	AdminPasswordHash string
}

// CommandRunner makes Docker interactions testable without a Docker daemon.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) (string, error)
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (osCommandRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	return string(output), err
}

// Launcher carries the replaceable parts of bootstrap for unit tests.
type Launcher struct {
	ConfigDir      string
	Runner         CommandRunner
	Random         io.Reader
	PromptPassword func() (string, error)
}

// New returns a launcher configured for the in-container /config volume.
func New() *Launcher {
	return &Launcher{
		ConfigDir: DefaultConfigDir,
		Runner:    osCommandRunner{},
		Random:    rand.Reader,
		PromptPassword: func() (string, error) {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return "", errors.New("admin password is required; run with -it or pass --admin-password-hash")
			}
			fmt.Fprint(os.Stderr, "Admin password: ")
			password, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", err
			}
			if len(password) == 0 {
				return "", errors.New("admin password cannot be empty")
			}
			return string(password), nil
		},
	}
}

func (l *Launcher) configDir() string {
	if l.ConfigDir == "" {
		return DefaultConfigDir
	}
	return l.ConfigDir
}

func (l *Launcher) runner() CommandRunner {
	if l.Runner == nil {
		return osCommandRunner{}
	}
	return l.Runner
}

func (l *Launcher) random() io.Reader {
	if l.Random == nil {
		return rand.Reader
	}
	return l.Random
}

func (l *Launcher) runSpecPath() string { return filepath.Join(l.configDir(), RunSpecFilename) }
func (l *Launcher) envPath() string     { return filepath.Join(l.configDir(), EnvFilename) }

// Up starts deploybot from existing launcher configuration, or performs the
// interactive first bootstrap before starting it. Existing config is never
// regenerated, because DEPLOYBOT_KEY encrypts the persistent database.
func (l *Launcher) Up(ctx context.Context, opts UpOptions) error {
	exists, err := l.configExists()
	if err != nil {
		return err
	}
	var spec RunSpec
	if exists {
		spec, err = l.Load()
		if err != nil {
			return err
		}
	} else {
		spec, err = l.bootstrap(opts)
		if err != nil {
			return err
		}
	}

	// docker rm -f is intentionally best-effort here. On a first boot there is
	// no container; if an existing container cannot be removed, docker run will
	// still fail with the useful name-conflict error.
	_ = l.runner().Run(ctx, "docker", "rm", "-f", spec.ContainerName)
	return l.runContainer(ctx, spec, spec.Image)
}

// Update pulls a target digest, swaps the old container, then persists enough
// state for a future rollback. It is run by a detached launcher container, so
// stopping deploybot cannot terminate this process.
func (l *Launcher) Update(ctx context.Context, targetDigest string) error {
	if !validDigest(targetDigest) {
		return fmt.Errorf("invalid target digest %q", targetDigest)
	}
	spec, err := l.Load()
	if err != nil {
		return err
	}
	targetImage := imageAtDigest(spec.Image, targetDigest)
	if err := l.runner().Run(ctx, "docker", "pull", targetImage); err != nil {
		return fmt.Errorf("pull %s: %w", targetImage, err)
	}

	previous := spec.CurrentImageDigest
	if previous == "" {
		previous, err = l.runningDigest(ctx, spec.ContainerName)
		if err != nil {
			return fmt.Errorf("read current image digest: %w", err)
		}
	}
	if err := l.runner().Run(ctx, "docker", "stop", spec.ContainerName); err != nil {
		return fmt.Errorf("stop %s: %w", spec.ContainerName, err)
	}
	if err := l.runner().Run(ctx, "docker", "rm", spec.ContainerName); err != nil {
		return fmt.Errorf("remove %s: %w", spec.ContainerName, err)
	}
	if err := l.runContainer(ctx, spec, targetImage); err != nil {
		return err
	}

	spec.PreviousImageDigest = previous
	spec.CurrentImageDigest = targetDigest
	if err := l.writeSpec(spec); err != nil {
		return fmt.Errorf("save launcher state: %w", err)
	}
	return nil
}

// Rollback swaps deploybot to the last digest recorded by Update.
func (l *Launcher) Rollback(ctx context.Context) error {
	spec, err := l.Load()
	if err != nil {
		return err
	}
	if spec.PreviousImageDigest == "" {
		return errors.New("no previous image digest is recorded")
	}
	return l.Update(ctx, spec.PreviousImageDigest)
}

// Load reads and validates the persistent run specification. It also requires
// the companion environment file, avoiding a half-written bootstrap from
// silently generating replacement encryption keys.
func (l *Launcher) Load() (RunSpec, error) {
	if _, err := os.Stat(l.envPath()); err != nil {
		return RunSpec{}, fmt.Errorf("launcher environment: %w", err)
	}
	data, err := os.ReadFile(l.runSpecPath())
	if err != nil {
		return RunSpec{}, fmt.Errorf("launcher run spec: %w", err)
	}
	var spec RunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return RunSpec{}, fmt.Errorf("parse launcher run spec: %w", err)
	}
	if err := validateSpec(spec); err != nil {
		return RunSpec{}, err
	}
	return spec, nil
}

func (l *Launcher) configExists() (bool, error) {
	_, specErr := os.Stat(l.runSpecPath())
	_, envErr := os.Stat(l.envPath())
	specExists := specErr == nil
	envExists := envErr == nil
	if specErr != nil && !os.IsNotExist(specErr) {
		return false, specErr
	}
	if envErr != nil && !os.IsNotExist(envErr) {
		return false, envErr
	}
	if specExists != envExists {
		return false, errors.New("incomplete launcher config: both run.json and deploybot.env are required")
	}
	return specExists, nil
}

func (l *Launcher) bootstrap(opts UpOptions) (RunSpec, error) {
	if opts.Image == "" {
		return RunSpec{}, errors.New("--image is required on first boot")
	}
	if opts.ConfigVolume == "" {
		opts.ConfigVolume = DefaultConfigVolume
	}
	if opts.DataVolume == "" {
		opts.DataVolume = DefaultDataVolume
	}
	if opts.ContainerName == "" {
		opts.ContainerName = DefaultContainer
	}
	if len(opts.Ports) == 0 {
		opts.Ports = []string{DefaultPort}
	}

	adminHash := opts.AdminPasswordHash
	if adminHash == "" {
		if l.PromptPassword == nil {
			return RunSpec{}, errors.New("admin password is required; pass --admin-password-hash")
		}
		password, err := l.PromptPassword()
		if err != nil {
			return RunSpec{}, err
		}
		adminHash, err = auth.HashPassword(password)
		if err != nil {
			return RunSpec{}, fmt.Errorf("hash admin password: %w", err)
		}
	}

	key, err := suppliedOrRandomKey(opts.EncryptionKey, l.random())
	if err != nil {
		return RunSpec{}, fmt.Errorf("DEPLOYBOT_KEY: %w", err)
	}
	sessionKey, err := suppliedOrRandomKey(opts.SessionKey, l.random())
	if err != nil {
		return RunSpec{}, fmt.Errorf("DEPLOYBOT_SESSION_KEY: %w", err)
	}
	spec := RunSpec{
		Image:         opts.Image,
		ContainerName: opts.ContainerName,
		Ports:         append([]string(nil), opts.Ports...),
		Volumes: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			opts.DataVolume + ":/data",
			opts.ConfigVolume + ":/config",
		},
		Labels: map[string]string{
			"deploybot.service": "deploybot",
		},
		Restart:      "unless-stopped",
		ConfigVolume: opts.ConfigVolume,
	}
	if err := validateSpec(spec); err != nil {
		return RunSpec{}, err
	}
	if err := os.MkdirAll(l.configDir(), 0o700); err != nil {
		return RunSpec{}, err
	}
	env := fmt.Sprintf("DEPLOYBOT_KEY=%s\nDEPLOYBOT_SESSION_KEY=%s\nDEPLOYBOT_ADMIN_HASH=%s\nDEPLOYBOT_DB=/data/deploybot.db\nDEPLOYBOT_LISTEN=:8080\nDEPLOYBOT_POLL_INTERVAL=60s\n", key, sessionKey, adminHash)
	if err := writeAtomic(l.envPath(), []byte(env), 0o600); err != nil {
		return RunSpec{}, fmt.Errorf("write launcher environment: %w", err)
	}
	if err := l.writeSpec(spec); err != nil {
		return RunSpec{}, err
	}
	return spec, nil
}

func (l *Launcher) runContainer(ctx context.Context, spec RunSpec, image string) error {
	args := []string{"run", "-d", "--name", spec.ContainerName, "--restart", spec.Restart, "--env-file", l.envPath()}
	for _, port := range spec.Ports {
		args = append(args, "-p", port)
	}
	for _, volume := range spec.Volumes {
		args = append(args, "-v", volume)
	}
	keys := make([]string, 0, len(spec.Labels))
	for key := range spec.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", key+"="+spec.Labels[key])
	}
	args = append(args,
		"-e", "DEPLOYBOT_CONFIG_VOLUME="+spec.ConfigVolume,
		"-e", "DEPLOYBOT_SELF_CONTAINER="+spec.ContainerName,
		"-e", "DEPLOYBOT_SELF_IMAGE="+spec.Image,
		image,
	)
	if err := l.runner().Run(ctx, "docker", args...); err != nil {
		return fmt.Errorf("start %s: %w", spec.ContainerName, err)
	}
	return nil
}

func (l *Launcher) runningDigest(ctx context.Context, containerName string) (string, error) {
	imageID, err := l.runner().Output(ctx, "docker", "inspect", "--format", "{{.Image}}", containerName)
	if err != nil {
		return "", err
	}
	imageID = strings.TrimSpace(imageID)
	if imageID == "" {
		return "", nil
	}
	repoDigest, err := l.runner().Output(ctx, "docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", imageID)
	if err != nil {
		return "", err
	}
	if i := strings.Index(strings.TrimSpace(repoDigest), "@"); i >= 0 {
		return strings.TrimSpace(repoDigest)[i+1:], nil
	}
	return "", nil
}

func (l *Launcher) writeSpec(spec RunSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(l.runSpecPath(), data, 0o600)
}

func validateSpec(spec RunSpec) error {
	if strings.TrimSpace(spec.Image) == "" {
		return errors.New("launcher run spec image is required")
	}
	if strings.TrimSpace(spec.ContainerName) == "" {
		return errors.New("launcher run spec container_name is required")
	}
	if strings.TrimSpace(spec.ConfigVolume) == "" {
		return errors.New("launcher run spec config_volume is required")
	}
	if strings.TrimSpace(spec.Restart) == "" {
		return errors.New("launcher run spec restart is required")
	}
	if len(spec.Ports) == 0 {
		return errors.New("launcher run spec requires at least one port")
	}
	if !contains(spec.Volumes, "/var/run/docker.sock:/var/run/docker.sock") {
		return errors.New("launcher run spec must mount /var/run/docker.sock")
	}
	if !contains(spec.Volumes, spec.ConfigVolume+":/config") {
		return fmt.Errorf("launcher run spec must mount %s:/config", spec.ConfigVolume)
	}
	if spec.Labels["deploybot.service"] != "deploybot" {
		return errors.New("launcher run spec must label deploybot.service=deploybot")
	}
	return nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func randomKey(source io.Reader) (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(source, buf); err != nil {
		return "", fmt.Errorf("generate random key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func suppliedOrRandomKey(value string, source io.Reader) (string, error) {
	if value == "" {
		return randomKey(source)
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	if len(decoded) != 32 {
		return "", fmt.Errorf("must decode to 32 bytes, got %d", len(decoded))
	}
	return value, nil
}

func validDigest(digest string) bool {
	return strings.HasPrefix(digest, "sha256:") && len(strings.TrimPrefix(digest, "sha256:")) > 0
}

func imageAtDigest(image, digest string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	return image + "@" + digest
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".deploybot-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
