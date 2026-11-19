package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"nori/internal/deploytemplate"
	"nori/internal/docker"
	"nori/internal/envfile"
	"nori/internal/executor"
	"nori/internal/store"
)

var dockerVolume = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

func ValidatePolicy(ctx context.Context, svc store.Service, policy store.BackupPolicy) error {
	if policy.Schedule != "" {
		if _, err := cronSchedule(policy.Schedule); err != nil {
			return errors.New("invalid backup cron expression")
		}
	}
	if policy.Keep < 1 || policy.Keep > 100 || len(policy.Sources) > 32 {
		return errors.New("retain 1 to 100 copies and select at most 32 sources")
	}
	if (policy.Quiesce == "") != (policy.Resume == "") || (policy.Export == "") != (policy.Import == "") {
		return errors.New("quiesce/resume and export/import commands must be provided in pairs")
	}
	if policy.StopApp && svc.DeploymentMode != store.DeploymentModePostgres && svc.DeploymentMode != store.DeploymentModeSingleContainer {
		return errors.New("automatic stop is only available for managed application containers")
	}
	for _, script := range []string{policy.Quiesce, policy.Resume, policy.Export, policy.Import} {
		if script != "" {
			if len(script) > 128<<10 || executor.ValidateScript(ctx, script) != nil {
				return errors.New("invalid or oversized backup command")
			}
		}
	}
	seen := make(map[store.BackupSource]bool)
	for _, source := range policy.Sources {
		if seen[source] {
			return errors.New("duplicate backup source")
		}
		seen[source] = true
		switch source.Type {
		case "volume", "managed":
			if !dockerVolume.MatchString(source.Path) {
				return errors.New("invalid Docker volume name")
			}
			if source.Type == "managed" {
				config, err := deploytemplate.ParseConfig(deploytemplate.Mode(svc.DeploymentMode), svc.TemplateConfig)
				if err != nil || !slices.ContainsFunc(config.Volumes, func(v deploytemplate.VolumeMount) bool { return v.Name == source.Path }) {
					return errors.New("managed volume must appear in the deployment template")
				}
			} else if svc.DeploymentMode == store.DeploymentModePostgres && source.Path == fmt.Sprintf("nori-%d-volume-postgres-data", svc.ID) {
				return errors.New("use the logical PostgreSQL source for the managed database")
			}
		case "bind":
			if !filepath.IsAbs(source.Path) || filepath.Clean(source.Path) != source.Path || source.Path == "/" ||
				strings.ContainsAny(source.Path, ",\r\n\x00") {
				return errors.New("bind source must be a clean absolute directory")
			}
		default:
			return errors.New("unknown backup source type")
		}
	}
	return nil
}

func (m *Manager) command(ctx context.Context, args ...string) *exec.Cmd {
	if m.dockerHost != "" {
		args = append([]string{"--host", m.dockerHost}, args...)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (m *Manager) dockerCommand(ctx context.Context, input io.Reader, output io.Writer, args ...string) error {
	cmd := m.command(ctx, args...)
	cmd.Stdin, cmd.Stdout = input, output
	if err := cmd.Run(); err != nil {
		// Docker and user tools can print credentials on stderr.
		return fmt.Errorf("Docker %s failed", args[0])
	}
	return nil
}

func (m *Manager) Discover(ctx context.Context, svc store.Service) ([]store.BackupSource, error) {
	if svc.IsSelf || m.docker == nil {
		return nil, nil
	}
	containers, err := m.docker.ListByService(ctx, svc.Name)
	if err != nil {
		return nil, err
	}
	seen := make(map[store.BackupSource]bool)
	for _, container := range containers {
		out, err := m.command(ctx, "container", "inspect", "--format", "{{json .Mounts}}", container.ID).Output()
		if err != nil {
			return nil, errors.New("could not inspect service mounts")
		}
		var mounts []struct{ Type, Name, Source string }
		if err := json.Unmarshal(out, &mounts); err != nil {
			return nil, err
		}
		for _, mount := range mounts {
			source := store.BackupSource{Type: mount.Type, Path: mount.Name}
			if mount.Type == "bind" {
				source.Path = mount.Source
			}
			if mount.Type == "volume" && svc.DeploymentMode != store.DeploymentModeCustom && svc.DeploymentMode != store.DeploymentModeManualScript {
				if logical, ok := strings.CutPrefix(mount.Name, fmt.Sprintf("nori-%d-volume-", svc.ID)); ok {
					source = store.BackupSource{Type: "managed", Path: logical}
				}
			}
			p := store.BackupPolicy{Keep: 1, Sources: []store.BackupSource{source}}
			if ValidatePolicy(ctx, svc, p) == nil {
				seen[source] = true
			}
		}
	}
	result := slices.Collect(maps.Keys(seen))
	slices.SortFunc(result, func(a, b store.BackupSource) int {
		return strings.Compare(a.Type+" "+a.Path, b.Type+" "+b.Path)
	})
	return result, nil
}

func managedPlan(svc store.Service) (deploytemplate.Plan, error) {
	config, err := deploytemplate.ParseConfig(deploytemplate.Mode(svc.DeploymentMode), svc.TemplateConfig)
	if err != nil {
		return deploytemplate.Plan{}, err
	}
	return deploytemplate.BuildDataPlan(svc.ID, svc.Name, config)
}

func (m *Manager) ownedContainer(ctx context.Context, resource deploytemplate.Resource) (docker.ManagedContainer, error) {
	if m.docker == nil {
		return docker.ManagedContainer{}, errors.New("Docker is unavailable")
	}
	got, err := m.docker.InspectContainer(ctx, resource.Name)
	if err != nil {
		return got, err
	}
	for key, expected := range resource.Labels {
		if key == "nori.role" && expected == "app" && got.Labels[key] == "candidate" {
			continue // Docker retains the candidate role after promotion.
		}
		if got.Labels[key] != expected {
			return got, docker.ErrOwnershipConflict
		}
	}
	return got, nil
}

type resumeAction struct{ App, Hook, Env string }

func (m *Manager) captureData(ctx context.Context, job store.BackupJob, manifest *Manifest, work string, check func() error) (result error) {
	svc, policy := manifest.Service, manifest.Policy
	if err := ValidatePolicy(ctx, svc, policy); err != nil {
		return err
	}
	var plan deploytemplate.Plan
	if svc.DeploymentMode == store.DeploymentModePostgres || svc.DeploymentMode == store.DeploymentModeSingleContainer {
		var err error
		plan, err = managedPlan(svc)
		if err != nil {
			return errors.New("could not build managed resource plan")
		}
	}

	action := resumeAction{Hook: policy.Resume}
	if action.Hook != "" {
		action.Env = manifest.Env
	}
	if policy.StopApp {
		app, err := m.ownedContainer(ctx, plan.AppCurrent)
		if err != nil && !errors.Is(err, docker.ErrManagedNotFound) {
			return errors.New("could not inspect the managed application")
		}
		if app.State == "running" {
			action.App = plan.AppCurrent.Name
		}
	}
	if action.App != "" || action.Hook != "" {
		raw, err := json.Marshal(action)
		if err != nil {
			return err
		}
		if err := m.store.WriteBackupSecret(ctx, resumeKey(job.ID), string(raw)); err != nil {
			return err
		}
		if err := m.store.SetBackupResume(ctx, job.ID, "pending"); err != nil {
			return err
		}
		defer func() {
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
			defer cancel()
			if err := m.resume(resumeCtx, job, action); err != nil {
				result = errors.Join(result, errors.New("could not resume service writers; Nori will retry"))
			}
		}()
		if action.App != "" {
			if err := m.docker.StopContainer(ctx, action.App); err != nil {
				return errors.New("could not stop the managed application")
			}
		}
		if policy.Quiesce != "" {
			if err := m.hook(ctx, svc.Name, manifest.Env, work, policy.Quiesce, check); err != nil {
				return errors.New("quiesce command failed")
			}
		}
	}

	if plan.Database != nil && !policy.SkipPostgres {
		info, err := m.capturePostgres(ctx, svc, plan, work, check)
		if err != nil {
			return err
		}
		if info != nil {
			manifest.Postgres = info
			manifest.Captured["postgres"] = time.Now().UTC()
		}
	}
	for index, source := range policy.Sources {
		resolved, err := m.resolveSource(ctx, svc, source)
		if err != nil {
			return err
		}
		file := filepath.Join(work, fmt.Sprintf("source-%d", index))
		err = writePayload(file, check, func(out io.Writer) error {
			return m.mountTar(ctx, resolved, nil, out)
		})
		if err != nil {
			return fmt.Errorf("backup source %d failed: %w", index+1, err)
		}
		manifest.Captured[fmt.Sprintf("source-%d", index)] = time.Now().UTC()
	}
	if policy.Export != "" {
		output, err := os.MkdirTemp(m.dir, ".nori-work-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(output)
		if err := m.hook(ctx, svc.Name, manifest.Env, output, policy.Export, check); err != nil {
			return errors.New("export command failed")
		}
		if err := writePayload(filepath.Join(work, "custom"), check, func(out io.Writer) error {
			cmd := exec.CommandContext(ctx, "tar", "-cpf", "-", "-C", output, ".")
			cmd.Stdout = out
			return cmd.Run()
		}); err != nil {
			return errors.New("could not archive custom export")
		}
		manifest.Captured["custom"] = time.Now().UTC()
	}
	return nil
}

func (m *Manager) capturePostgres(ctx context.Context, svc store.Service, plan deploytemplate.Plan, work string, check func() error) (*PostgresInfo, error) {
	db, err := m.ownedContainer(ctx, *plan.Database)
	if errors.Is(err, docker.ErrManagedNotFound) {
		_, stateErr := m.store.GetTemplateState(ctx, svc.ID)
		_, volumeErr := m.docker.InspectVolume(ctx, fmt.Sprintf("nori-%d-volume-postgres-data", svc.ID))
		if errors.Is(stateErr, store.ErrNotFound) && errors.Is(volumeErr, docker.ErrManagedNotFound) {
			return nil, nil
		}
	}
	if err != nil || db.State != "running" {
		return nil, errors.New("managed PostgreSQL must be available to create a backup")
	}
	info, err := m.postgresInfo(ctx, plan, false)
	if err != nil {
		return nil, err
	}
	err = writePayload(filepath.Join(work, "postgres"), check, func(out io.Writer) error {
		return m.postgresCommand(ctx, plan, nil, out, "pg_dump", "--format=custom")
	})
	return &info, err
}

func (m *Manager) resolveSource(ctx context.Context, svc store.Service, source store.BackupSource) (store.BackupSource, error) {
	if source.Type == "managed" {
		plan, err := managedPlan(svc)
		if err != nil {
			return source, err
		}
		name := fmt.Sprintf("nori-%d-volume-%s", svc.ID, source.Path)
		for _, volume := range plan.Volumes {
			if volume.Name == name {
				if m.docker == nil {
					return source, errors.New("Docker is unavailable")
				}
				got, err := m.docker.InspectVolume(ctx, name)
				if err != nil || !sameLabels(got.Labels, volume.Labels) {
					return source, errors.New("managed backup volume missing or ownership changed")
				}
				return store.BackupSource{Type: "volume", Path: name}, nil
			}
		}
		return source, errors.New("managed volume is not in the deployment plan")
	}
	if source.Type == "volume" {
		if m.docker == nil {
			return source, errors.New("Docker is unavailable")
		}
		if _, err := m.docker.InspectVolume(ctx, source.Path); err != nil {
			return source, errors.New("backup volume is missing or inaccessible")
		}
	}
	return source, nil
}

func (m *Manager) mountTar(ctx context.Context, source store.BackupSource, input io.Reader, output io.Writer) error {
	name := fmt.Sprintf("nori-backup-%d", time.Now().UnixNano())
	mount := "type=" + source.Type + ",src=" + source.Path + ",dst=/contents"
	flags := []string{"run", "--rm", "--name", name, "--network", "none", "--read-only", "--mount"}
	if input == nil {
		flags = append(flags, mount+",readonly", "alpine:3.21", "tar", "-cpf", "-", "-C", "/contents", ".")
	} else {
		flags = append(flags, mount, "-i", "alpine:3.21", "tar", "-xpf", "-", "-C", "/contents")
	}
	if err := m.dockerCommand(ctx, input, output, flags...); err != nil {
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = m.dockerCommand(clean, nil, nil, "rm", "-f", name)
		return err
	}
	return nil
}

func writePayload(file string, check func() error, write func(io.Writer) error) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	return errors.Join(write(checkedWriter{writer: f, check: check}), f.Close())
}

func (m *Manager) hook(parent context.Context, service, dotenv, dir, script string, check func() error) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	env, err := envfile.Parse(dotenv)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "bash", "-e", "-o", "pipefail", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env, "SERVICE="+service, "NORI_BACKUP_WORK="+dir)
	if m.dockerHost != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+m.dockerHost)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if err := check(); err != nil {
				cancel()
				<-done
				return err
			}
		}
	}
}

func resumeKey(id int64) string { return fmt.Sprintf("backup_resume:%d", id) }

func (m *Manager) resume(ctx context.Context, job store.BackupJob, action resumeAction) error {
	if action.App != "" {
		resource := deploytemplate.Resource{Name: action.App, Labels: map[string]string{
			"nori.service": job.ServiceName, "nori.service-id": fmt.Sprint(job.ServiceID), "nori.template": "1", "nori.role": "app",
		}}
		app, err := m.ownedContainer(ctx, resource)
		if err != nil {
			return err
		}
		if app.State != "running" {
			if err := m.docker.StartContainer(ctx, action.App); err != nil {
				return err
			}
		}
	}
	if action.Hook != "" {
		if err := m.hook(ctx, job.ServiceName, action.Env, m.dir, action.Hook, ctx.Err); err != nil {
			return err
		}
	}
	if err := m.store.SetBackupResume(ctx, job.ID, ""); err != nil {
		return err
	}
	return m.store.DeleteBackupSecret(ctx, resumeKey(job.ID))
}

func (m *Manager) recoverWriters(ctx context.Context) {
	jobs, err := m.store.BackupsToResume(ctx)
	if err != nil {
		return
	}
	for _, job := range jobs {
		raw, err := m.store.ReadBackupSecret(ctx, resumeKey(job.ID))
		var action resumeAction
		if err == nil {
			err = json.Unmarshal([]byte(raw), &action)
		}
		if err == nil {
			child, cancel := context.WithTimeout(ctx, 10*time.Minute)
			err = m.resume(child, job, action)
			cancel()
		}
		if err != nil {
			// Never log hook output or a parsed secret.
			fmt.Fprintf(os.Stderr, "backup: could not recover writers for backup %d\n", job.ID)
		}
	}
}

// Validate a nested tar before passing it to a Docker helper or extracting it.
// Only regular files, directories and in-tree symlinks can be restored.
func validateDataTar(file *os.File) error {
	r := tar.NewReader(file)
	seen := make(map[string]byte)
	for {
		header, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(header.Name)
		if !safeTarPath(header.Name) || len(seen) >= 1_000_000 {
			return errors.New("unsafe path or too many files in data archive")
		}
		if _, ok := seen[name]; ok {
			return errors.New("duplicate path in data archive")
		}
		seen[name] = header.Typeflag
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		case tar.TypeSymlink:
			if path.IsAbs(header.Linkname) || !safeTarPath(path.Join(path.Dir(name), header.Linkname)) {
				return errors.New("symbolic link leaves data archive")
			}
		case tar.TypeLink:
			if !safeTarPath(header.Linkname) || seen[path.Clean(header.Linkname)] != tar.TypeReg {
				return errors.New("invalid hard link in data archive")
			}
		default:
			return errors.New("unsupported data archive file type")
		}
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if kind, ok := seen[parent]; ok && kind != tar.TypeDir {
				return errors.New("archive path traverses a non-directory")
			}
		}
	}
	_, err := file.Seek(0, io.SeekStart)
	return err
}

func safeTarPath(name string) bool {
	if name == "" || path.IsAbs(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

type PostgresInfo struct {
	ServerVersion int
	Extensions    []PostgresExtension
}

type PostgresExtension struct {
	Name    string
	Version string
}

func (m *Manager) postgresCommand(ctx context.Context, plan deploytemplate.Plan, input io.Reader, output io.Writer, tool string, options ...string) error {
	const init = `export PGPASSWORD="$(printenv "$1")"; shift; exec "$@"`
	args := []string{"exec"}
	if input != nil {
		args = append(args, "-i")
	}
	args = append(args, "--user", "postgres", plan.Database.Name, "sh", "-ec", init, "nori-pg", plan.Postgres.PasswordEnv,
		tool, "--host", "127.0.0.1", "--no-password", "--username", plan.Postgres.User, "--dbname", plan.Postgres.Database)
	return m.dockerCommand(ctx, input, output, append(args, options...)...)
}

func (m *Manager) postgresInfo(ctx context.Context, plan deploytemplate.Plan, available bool) (PostgresInfo, error) {
	extensions := "SELECT extname AS name, extversion AS version FROM pg_extension"
	if available {
		extensions = "SELECT name, version FROM pg_available_extension_versions"
	}
	query := "SELECT json_build_object('ServerVersion', current_setting('server_version_num')::int, 'Extensions', " +
		"(SELECT json_agg(e) FROM (" + extensions + ") e))"
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var info PostgresInfo
	if err := m.postgresCommand(ctx, plan, nil, &output, "psql", "-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", query); err != nil {
		return info, errors.New("could not read PostgreSQL version and extensions")
	}
	if err := json.Unmarshal(output.Bytes(), &info); err != nil || info.ServerVersion <= 0 {
		return info, errors.New("invalid PostgreSQL version metadata")
	}
	return info, nil
}
