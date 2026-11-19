package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/joho/godotenv"

	"nori/internal/deploytemplate"
	"nori/internal/docker"
	"nori/internal/envfile"
	"nori/internal/executor"
	"nori/internal/store"
)

type RestoreOptions struct {
	Name           string
	Network        string
	Domain         string
	Targets        map[int]string
	ReviewCommands bool
	DeployScript   *string
}

var restoredServiceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

func (m *Manager) Restore(ctx context.Context, manifest Manifest, dir string, options RestoreOptions) (store.Service, error) {
	if !m.restoreMu.TryLock() {
		return store.Service{}, errors.New("another restore or its cleanup is in progress")
	}
	defer m.restoreMu.Unlock()
	source := manifest.Service
	svc := source
	svc.ID, svc.ConfigVersion = 0, 0
	svc.Name, svc.Policy, svc.CronExpr = options.Name, store.PolicyManual, ""
	if !restoredServiceName.MatchString(svc.Name) || strings.EqualFold(svc.Name, store.SelfServiceName) || svc.IsSelf {
		return svc, errors.New("enter a valid new service name")
	}
	if _, err := m.store.GetServiceByName(ctx, svc.Name); !errors.Is(err, store.ErrNotFound) {
		if err != nil {
			return svc, errors.New("could not check whether the destination service exists")
		}
		return svc, errors.New("a service with that name already exists")
	}
	if _, err := envfile.Parse(manifest.Env); err != nil {
		return svc, errors.New("invalid dotenv in backup")
	}

	policy := manifest.Policy
	policy.Sources = slices.Clone(policy.Sources)
	hasHooks := policy.Quiesce != "" || policy.Resume != "" || policy.Export != "" || policy.Import != ""
	if hasHooks && !options.ReviewCommands {
		if _, custom := manifest.Files["custom"]; custom {
			return svc, errors.New("review and approve the custom import command to restore its data")
		}
		policy.Quiesce, policy.Resume, policy.Export, policy.Import = "", "", "", ""
	}
	for i, source := range policy.Sources {
		if source.Type != "managed" {
			value := options.Targets[i]
			if value == "" {
				return svc, errors.New("choose a fresh destination for each unmanaged volume and bind mount")
			}
			policy.Sources[i].Path = value
		}
	}
	if options.DeployScript != nil {
		svc.DeployScript = *options.DeployScript
	}
	var config deploytemplate.Config
	switch svc.DeploymentMode {
	case store.DeploymentModePostgres, store.DeploymentModeSingleContainer:
		var err error
		config, err = deploytemplate.ParseConfig(deploytemplate.Mode(svc.DeploymentMode), svc.TemplateConfig)
		if err != nil {
			return svc, errors.New("invalid deployment template in backup")
		}
		if config.Proxy != nil {
			if options.Network != "" {
				config.Proxy.Network = options.Network
			}
			if options.Domain != "" {
				config.Proxy.Domain = options.Domain
			}
		} else if options.Network != "" {
			config.ServingNetwork = options.Network
		}
		if err := m.checkProxyConflict(ctx, config); err != nil {
			return svc, err
		}
		if err := config.Validate(); err != nil {
			return svc, errors.New("invalid destination template configuration")
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return svc, err
		}
		svc.TemplateConfig = string(raw)
	case store.DeploymentModeCustom, store.DeploymentModeManualScript:
		if executor.ValidateScript(ctx, svc.DeployScript) != nil {
			return svc, errors.New("invalid deployment script in backup")
		}
	default:
		return svc, errors.New("unsupported deployment mode")
	}
	if err := ValidatePolicy(ctx, svc, policy); err != nil {
		return svc, err
	}
	if err := validatePayloads(manifest, dir); err != nil {
		return svc, err
	}
	if err := m.preflightRestore(ctx, svc, manifest.Digest, config, policy); err != nil {
		return svc, err
	}
	dotenv := manifest.Env
	token := rand.Text()
	err := m.store.SaveRestoredService(ctx, &svc, dotenv, policy, token, func(id int64, env string) (string, error) {
		if config.Postgres != nil && (manifest.Postgres != nil || hasEnv(env, config.Postgres.ConnectionURLEnv)) {
			var err error
			dotenv, err = remapPostgresEnv(id, source.ID, env, *config.Postgres)
			if err != nil {
				return "", err
			}
		}
		return dotenv, nil
	})
	if err != nil {
		return svc, errors.New("could not save the new service configuration")
	}
	if err := m.restoreData(ctx, svc, dotenv, manifest, policy, dir, token); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if cleanupErr := m.discardRestore(cleanupCtx, store.RestoreAttempt{ServiceID: svc.ID, Token: token}); cleanupErr != nil {
			return svc, fmt.Errorf("%w; new resources could not all be removed", err)
		}
		return svc, err
	}
	return svc, m.store.FinishRestore(ctx, svc.ID)
}

func remapPostgresEnv(target, original int64, content string, config deploytemplate.PostgresConfig) (string, error) {
	values, err := godotenv.Unmarshal(content)
	if err != nil {
		return "", errors.New("invalid PostgreSQL environment")
	}
	u, err := url.Parse(values[config.ConnectionURLEnv])
	if err != nil || u.Hostname() != fmt.Sprintf("nori-%d-postgres", original) {
		return "", errors.New("backup has an invalid PostgreSQL connection URL")
	}
	port := u.Port()
	u.Host = fmt.Sprintf("nori-%d-postgres", target)
	if port != "" {
		u.Host = net.JoinHostPort(u.Host, port)
	}
	return envfile.SetValue(content, config.ConnectionURLEnv, u.String())
}

func validatePayloads(manifest Manifest, dir string) error {
	for name := range manifest.Files {
		switch name {
		case "postgres":
			if manifest.Service.DeploymentMode != store.DeploymentModePostgres || manifest.Policy.SkipPostgres {
				return errors.New("unexpected PostgreSQL payload")
			}
			continue
		case "custom":
			if manifest.Policy.Import == "" {
				return errors.New("custom payload has no import command")
			}
		default:
			raw, ok := strings.CutPrefix(name, "source-")
			index, err := strconv.Atoi(raw)
			if !ok || err != nil || index < 0 || index >= len(manifest.Policy.Sources) || name != fmt.Sprintf("source-%d", index) {
				return errors.New("unknown backup payload")
			}
		}
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if err := errors.Join(validateDataTar(file), file.Close()); err != nil {
			return fmt.Errorf("invalid %s payload: %w", name, err)
		}
	}
	for i := range manifest.Policy.Sources {
		if _, exists := manifest.Files[fmt.Sprintf("source-%d", i)]; !exists {
			return errors.New("backup is missing a selected data source")
		}
	}
	return nil
}

func (m *Manager) preflightRestore(ctx context.Context, svc store.Service, digest string, config deploytemplate.Config, policy store.BackupPolicy) error {
	if svc.WatchedImage != "" && m.docker != nil {
		image, err := reference.ParseNormalizedNamed(svc.WatchedImage)
		if err != nil {
			return errors.New("backup contains an invalid watched image")
		}
		if digest != "" {
			image, err = reference.ParseNormalizedNamed(reference.TrimNamed(image).Name() + "@" + digest)
			if err != nil {
				return errors.New("backup contains an invalid running image digest")
			}
		}
		if err := m.docker.Pull(ctx, image.String()); err != nil {
			return errors.New("the destination Docker host could not pull the backed-up image")
		}
	}
	network := config.ServingNetwork
	if config.Proxy != nil {
		network = config.Proxy.Network
	}
	if network != "" {
		if m.docker == nil {
			return errors.New("Docker is unavailable for network preflight")
		}
		if _, err := m.docker.InspectNetwork(ctx, network); err != nil {
			return errors.New("destination serving network does not exist")
		}
	}
	for _, target := range policy.Sources {
		if target.Type == "volume" {
			if m.docker == nil {
				return errors.New("Docker is unavailable for volume preflight")
			}
			if _, err := m.docker.InspectVolume(ctx, target.Path); !errors.Is(err, docker.ErrManagedNotFound) {
				return errors.New("destination Docker volume already exists or could not be inspected")
			}
		}
	}
	return nil
}

func (m *Manager) restoreData(ctx context.Context, svc store.Service, dotenv string, manifest Manifest, policy store.BackupPolicy, dir string, token string) error {
	files := manifest.Files
	if len(files) == 0 {
		return nil
	}
	check, err := m.diskGuard(ctx)
	if err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	var plan deploytemplate.Plan
	if svc.DeploymentMode == store.DeploymentModePostgres || svc.DeploymentMode == store.DeploymentModeSingleContainer {
		plan, err = managedPlan(svc)
		if err != nil {
			return errors.New("could not build the destination deployment plan")
		}
		provisioner, ok := m.coord.(interface {
			PrepareRestore(context.Context, *store.Service, deploytemplate.Plan, string) error
		})
		if !ok || m.docker == nil {
			return errors.New("managed restore requires Docker")
		}
		if err := m.requireFreshManaged(ctx, plan); err != nil {
			return err
		}
		labelRestorePlan(&plan, token)
		if err := provisioner.PrepareRestore(ctx, &svc, plan, dotenv); err != nil {
			return errors.New("could not initialize fresh managed resources")
		}
	}
	if _, ok := files["postgres"]; ok {
		file, err := os.Open(filepath.Join(dir, "postgres"))
		if err != nil {
			return err
		}
		defer file.Close()
		if manifest.Postgres == nil {
			return errors.New("PostgreSQL payload is missing version metadata")
		}
		targetInfo, err := m.postgresInfo(ctx, plan, true)
		if err != nil {
			return err
		}
		if targetInfo.ServerVersion/10000 < manifest.Postgres.ServerVersion/10000 {
			return errors.New("destination PostgreSQL major version is older than the source")
		}
		for _, extension := range manifest.Postgres.Extensions {
			if !slices.Contains(targetInfo.Extensions, extension) {
				return fmt.Errorf("destination PostgreSQL is missing extension %s version %s", extension.Name, extension.Version)
			}
		}
		if err := watchDisk(ctx, check, func(ctx context.Context) error {
			return m.postgresCommand(ctx, plan, file, nil, "pg_restore",
				"--single-transaction", "--exit-on-error", "--no-owner", "--no-privileges")
		}); err != nil {
			return errors.New("could not restore PostgreSQL data; check database compatibility and installed extensions")
		}
	}
	for i, source := range policy.Sources {
		if err := check(); err != nil {
			return err
		}
		target, err := m.createTarget(ctx, svc, source, token)
		if err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(dir, fmt.Sprintf("source-%d", i)))
		if err != nil {
			return err
		}
		err = watchDisk(ctx, check, func(ctx context.Context) error { return m.mountTar(ctx, target, f, nil) })
		f.Close()
		if err != nil {
			return fmt.Errorf("could not restore data to %s; inspect the fresh target before retrying", target.Path)
		}
	}
	if _, ok := files["custom"]; ok {
		unpack := filepath.Join(dir, "custom-files")
		if err := os.Mkdir(unpack, 0700); err != nil {
			return err
		}
		if err := watchDisk(ctx, check, func(ctx context.Context) error {
			return exec.CommandContext(ctx, "tar", "-xf", filepath.Join(dir, "custom"), "-C", unpack).Run()
		}); err != nil {
			return errors.New("could not unpack custom data")
		}
		if err := m.hook(ctx, svc.Name, dotenv, unpack, policy.Import, check); err != nil {
			return errors.New("custom import command failed; inspect its destination before retrying")
		}
	}
	return nil
}

func (m *Manager) requireFreshManaged(ctx context.Context, plan deploytemplate.Plan) error {
	for _, volume := range plan.Volumes {
		if _, err := m.docker.InspectVolume(ctx, volume.Name); !errors.Is(err, docker.ErrManagedNotFound) {
			return errors.New("managed destination volume already exists or could not be inspected")
		}
	}
	if plan.InternalNetwork != nil {
		if _, err := m.docker.InspectNetwork(ctx, plan.InternalNetwork.Name); !errors.Is(err, docker.ErrManagedNotFound) {
			return errors.New("managed destination network already exists or could not be inspected")
		}
	}
	if plan.Database != nil {
		if _, err := m.docker.InspectContainer(ctx, plan.Database.Name); !errors.Is(err, docker.ErrManagedNotFound) {
			return errors.New("managed destination DB container already exists or could not be inspected")
		}
	}
	return nil
}

func (m *Manager) createTarget(ctx context.Context, svc store.Service, source store.BackupSource, token string) (store.BackupSource, error) {
	switch source.Type {
	case "managed":
		return m.resolveSource(ctx, svc, source)
	case "volume":
		res := docker.ManagedResource{Name: source.Path, Labels: map[string]string{restoreLabel: token}}
		if err := m.docker.EnsureVolume(ctx, res); err != nil {
			return source, errors.New("could not create fresh destination volume")
		}
		return source, nil
	case "bind":
		parent, base := filepath.Dir(source.Path), filepath.Base(source.Path)
		err := m.dockerCommand(ctx, nil, io.Discard, "run", "--rm", "--network", "none", "--read-only", "--mount",
			"type=bind,src="+parent+",dst=/contents", "alpine:3.21", "mkdir", "--", "/contents/"+base)
		if err != nil {
			return source, errors.New("destination bind directory already exists or its parent cannot be written")
		}
		return source, nil
	}
	return source, errors.New("unsupported restore target")
}

const restoreLabel = "nori.backup-restore"

func labelRestorePlan(plan *deploytemplate.Plan, token string) {
	for _, volume := range plan.Volumes {
		volume.Labels[restoreLabel] = token
	}
	if plan.Database != nil {
		plan.Database.Labels[restoreLabel] = token
	}
	if plan.InternalNetwork != nil {
		plan.InternalNetwork.Labels[restoreLabel] = token
	}
}

func (m *Manager) discardRestore(ctx context.Context, attempt store.RestoreAttempt) error {
	svc, err := m.store.GetService(ctx, attempt.ServiceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	policy, _, err := m.store.BackupPolicy(ctx, svc.ID)
	if err != nil {
		return err
	}
	if err := m.cleanupRestore(ctx, *svc, policy, attempt.Token); err != nil {
		return err
	}
	return m.store.DeleteIncompleteRestore(ctx, svc.ID, attempt.Token)
}

func (m *Manager) cleanupRestore(ctx context.Context, svc store.Service, policy store.BackupPolicy, token string) error {
	var plan deploytemplate.Plan
	if svc.DeploymentMode == store.DeploymentModePostgres || svc.DeploymentMode == store.DeploymentModeSingleContainer {
		var err error
		plan, err = managedPlan(svc)
		if err != nil {
			return err
		}
	}
	volumes := make([]string, 0, len(plan.Volumes)+len(policy.Sources))
	for _, v := range plan.Volumes {
		volumes = append(volumes, v.Name)
	}
	for _, source := range policy.Sources {
		if source.Type == "volume" {
			volumes = append(volumes, source.Path)
		}
	}
	if m.docker == nil {
		if len(volumes) != 0 || plan.InternalNetwork != nil || plan.Database != nil {
			return errors.New("Docker is unavailable for restore cleanup")
		}
		return nil
	}
	var cleanupErr error
	if plan.Database != nil {
		db, err := m.docker.InspectContainer(ctx, plan.Database.Name)
		switch {
		case err == nil && db.Labels[restoreLabel] == token:
			cleanupErr = errors.Join(cleanupErr, m.docker.RemoveContainer(ctx, plan.Database.Name))
		case err != nil && !errors.Is(err, docker.ErrManagedNotFound):
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	for _, volume := range volumes {
		got, err := m.docker.InspectVolume(ctx, volume)
		switch {
		case err == nil && got.Labels[restoreLabel] == token:
			cleanupErr = errors.Join(cleanupErr, m.dockerCommand(ctx, nil, nil, "volume", "rm", volume))
		case err != nil && !errors.Is(err, docker.ErrManagedNotFound):
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if plan.InternalNetwork != nil {
		name := plan.InternalNetwork.Name
		got, err := m.docker.InspectNetwork(ctx, name)
		switch {
		case err == nil && got.Labels[restoreLabel] == token:
			cleanupErr = errors.Join(cleanupErr, m.dockerCommand(ctx, nil, nil, "network", "rm", name))
		case err != nil && !errors.Is(err, docker.ErrManagedNotFound):
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

func (m *Manager) checkProxyConflict(ctx context.Context, config deploytemplate.Config) error {
	if config.Proxy == nil {
		return nil
	}
	services, err := m.store.ListServices(ctx)
	if err != nil {
		return errors.New("could not check the destination proxy domain")
	}
	for _, service := range services {
		if service.DeploymentMode != store.DeploymentModePostgres && service.DeploymentMode != store.DeploymentModeSingleContainer {
			continue
		}
		other, err := deploytemplate.ParseConfig(deploytemplate.Mode(service.DeploymentMode), service.TemplateConfig)
		if err == nil && other.Proxy != nil && strings.EqualFold(other.Proxy.Domain, config.Proxy.Domain) {
			return errors.New("choose a different proxy domain to test the restored service alongside the original")
		}
	}
	return nil
}

func hasEnv(content, key string) bool {
	values, err := godotenv.Unmarshal(content)
	return err == nil && values[key] != ""
}

func sameLabels(actual, required map[string]string) bool {
	for k, v := range required {
		if actual[k] != v {
			return false
		}
	}
	return true
}
