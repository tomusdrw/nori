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
	"regexp"
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

var validEnvironmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	Network             string            `json:"network,omitempty"`
	ConfigVolume        string            `json:"config_volume"`
	CurrentImageDigest  string            `json:"current_image_digest,omitempty"`
	PreviousImageDigest string            `json:"previous_image_digest,omitempty"`
}

// UpOptions describe bootstrap settings plus explicit persistent overrides.
// Existing launcher config remains authoritative unless an override is supplied.
type UpOptions struct {
	Image             string
	ConfigVolume      string
	ContainerName     string
	DataVolume        string
	Ports             []string
	NoPort            bool
	Network           string
	Volumes           []string
	Environment       []string
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
// regenerated; explicit override flags may update non-secret launch settings.
// DEPLOYBOT_KEY must stay stable because it encrypts the persistent database.
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
		if err := l.applyOverrides(&spec, opts); err != nil {
			return err
		}
	} else {
		spec, err = l.bootstrap(opts)
		if err != nil {
			return err
		}
	}

	// Removing an existing container is best-effort. Checking first avoids a
	// misleading "No such container" message during a normal first boot.
	if _, err := l.runner().Output(ctx, "docker", "container", "inspect", "--format", "{{.Id}}", spec.ContainerName); err == nil {
		_ = l.runner().Run(ctx, "docker", "rm", "-f", spec.ContainerName)
	}
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
	if opts.NoPort && len(opts.Ports) > 0 {
		return RunSpec{}, errors.New("--no-port cannot be combined with --port")
	}
	if len(opts.Ports) == 0 && !opts.NoPort {
		opts.Ports = []string{DefaultPort}
	}
	if err := validateVolumeMounts(opts.Volumes); err != nil {
		return RunSpec{}, err
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
		Volumes: appendVolumes([]string{
			"/var/run/docker.sock:/var/run/docker.sock",
			opts.DataVolume + ":/data",
			opts.ConfigVolume + ":/config",
		}, opts.Volumes),
		Labels: map[string]string{
			"deploybot.service": "deploybot",
		},
		Restart:      "unless-stopped",
		Network:      opts.Network,
		ConfigVolume: opts.ConfigVolume,
	}
	if err := validateSpec(spec); err != nil {
		return RunSpec{}, err
	}
	if err := os.MkdirAll(l.configDir(), 0o700); err != nil {
		return RunSpec{}, err
	}
	values := map[string]string{
		"DEPLOYBOT_KEY":           key,
		"DEPLOYBOT_SESSION_KEY":   sessionKey,
		"DEPLOYBOT_ADMIN_HASH":    adminHash,
		"DEPLOYBOT_DB":            "/data/deploybot.db",
		"DEPLOYBOT_LISTEN":        ":8080",
		"DEPLOYBOT_POLL_INTERVAL": "60s",
	}
	if err := mergeEnvironment(values, opts.Environment); err != nil {
		return RunSpec{}, err
	}
	env, err := marshalEnvironment(values)
	if err != nil {
		return RunSpec{}, fmt.Errorf("encode launcher environment: %w", err)
	}
	if err := writeAtomic(l.envPath(), env, 0o600); err != nil {
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
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
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

// applyOverrides updates only explicit first-class configuration flags. It is
// useful for repairing a failed first boot (for example, when a reverse proxy
// owns port 8080) without ever regenerating the persistent secret material.
func (l *Launcher) applyOverrides(spec *RunSpec, opts UpOptions) error {
	if opts.NoPort && len(opts.Ports) > 0 {
		return errors.New("--no-port cannot be combined with --port")
	}
	changed := false
	if opts.NoPort {
		spec.Ports = nil
		changed = true
	} else if len(opts.Ports) > 0 {
		spec.Ports = append([]string(nil), opts.Ports...)
		changed = true
	}
	if opts.Network != "" {
		spec.Network = opts.Network
		changed = true
	}
	if len(opts.Volumes) > 0 {
		if err := validateVolumeMounts(opts.Volumes); err != nil {
			return err
		}
		merged := appendVolumes(spec.Volumes, opts.Volumes)
		if len(merged) != len(spec.Volumes) {
			spec.Volumes = merged
			changed = true
		}
	}
	if len(opts.Environment) > 0 {
		if err := l.mergeEnvironment(opts.Environment); err != nil {
			return err
		}
	}
	if changed {
		if err := l.writeSpec(*spec); err != nil {
			return fmt.Errorf("save launcher configuration: %w", err)
		}
	}
	return nil
}

func (l *Launcher) mergeEnvironment(entries []string) error {
	data, err := os.ReadFile(l.envPath())
	if err != nil {
		return fmt.Errorf("read launcher environment: %w", err)
	}
	values, err := parseEnvironment(string(data))
	if err != nil {
		return fmt.Errorf("parse launcher environment: %w", err)
	}
	if err := mergeEnvironment(values, entries); err != nil {
		return err
	}
	encoded, err := marshalEnvironment(values)
	if err != nil {
		return fmt.Errorf("encode launcher environment: %w", err)
	}
	return writeAtomic(l.envPath(), encoded, 0o600)
}

func mergeEnvironment(values map[string]string, entries []string) error {
	for _, entry := range entries {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			return fmt.Errorf("invalid --env value %q: expected KEY=VALUE", entry)
		}
		if !validEnvironmentName.MatchString(key) {
			return fmt.Errorf("invalid --env variable name %q", key)
		}
		if err := validateEnvironmentValue(value); err != nil {
			return fmt.Errorf("invalid --env value for %s: %w", key, err)
		}
		if isProtectedEnvironmentKey(key) {
			return fmt.Errorf("%s is launcher-managed and cannot be set with --env", key)
		}
		values[key] = value
	}
	return nil
}

// parseEnvironment reads Docker's simple KEY=VALUE env-file format. Values
// deliberately remain raw: unlike shell dotenv parsers, Docker neither expands
// dollar signs nor strips quotes. That preserves existing bcrypt hashes.
func parseEnvironment(content string) (map[string]string, error) {
	values := make(map[string]string)
	for lineNumber, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNumber+1)
		}
		key = strings.TrimSpace(key)
		if !validEnvironmentName.MatchString(key) {
			return nil, fmt.Errorf("line %d: invalid environment variable name %q", lineNumber+1, key)
		}
		if err := validateEnvironmentValue(value); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		values[key] = value
	}
	return values, nil
}

func marshalEnvironment(values map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(values))
	for key := range values {
		if !validEnvironmentName.MatchString(key) {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var content strings.Builder
	for _, key := range keys {
		value := values[key]
		if err := validateEnvironmentValue(value); err != nil {
			return nil, fmt.Errorf("invalid value for %s: %w", key, err)
		}
		content.WriteString(key)
		content.WriteByte('=')
		content.WriteString(value)
		content.WriteByte('\n')
	}
	return []byte(content.String()), nil
}

func validateEnvironmentValue(value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("environment variable values cannot contain NUL, carriage return, or newline")
	}
	return nil
}

func isProtectedEnvironmentKey(key string) bool {
	switch key {
	case "DEPLOYBOT_KEY", "DEPLOYBOT_SESSION_KEY", "DEPLOYBOT_ADMIN_HASH", "DEPLOYBOT_CONFIG_VOLUME", "DEPLOYBOT_SELF_CONTAINER", "DEPLOYBOT_SELF_IMAGE":
		return true
	default:
		return false
	}
}

// appendVolumes returns existing with each extra mount that is not already
// present appended, preserving order. Deduplication keeps repeated `up`
// invocations idempotent so Docker never sees a duplicate mount point.
func appendVolumes(existing, extra []string) []string {
	out := existing
	for _, v := range extra {
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// validateVolumeMounts rejects entries that would corrupt the docker run
// invocation or the persisted run spec. Values are otherwise passed to
// `docker run -v` verbatim, so their src:dst[:opts] shape is Docker's to parse.
func validateVolumeMounts(volumes []string) error {
	for _, v := range volumes {
		if strings.TrimSpace(v) == "" {
			return errors.New("volume mount cannot be empty")
		}
		if strings.ContainsAny(v, "\x00\r\n") {
			return errors.New("volume mount cannot contain NUL, carriage return, or newline")
		}
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
