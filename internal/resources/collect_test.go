package resources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"nori/internal/docker"
	"nori/internal/store"
)

type resourceFixture struct {
	docker.Fake
	containers    []container.Summary
	volumes       []*volume.Volume
	stats         map[string]docker.RuntimeStats
	postgresCalls int
}

func (f *resourceFixture) ResourceHostName(context.Context) (string, error) { return os.Hostname() }
func (f *resourceFixture) ResourceContainers(context.Context) ([]container.Summary, error) {
	return f.containers, f.Err
}
func (f *resourceFixture) ResourceVolumes(context.Context) ([]*volume.Volume, error) {
	return f.volumes, nil
}
func (f *resourceFixture) ResourceStats(_ context.Context, id string) (docker.RuntimeStats, error) {
	return f.stats[id], nil
}
func (f *resourceFixture) ResourceStorage(context.Context, string) (docker.ContainerStorage, error) {
	n := int64(4)
	return docker.ContainerStorage{Writable: &n, LogDriver: "none"}, nil
}
func (f *resourceFixture) PostgresSizes(context.Context, string, string, string) ([]docker.PostgresSize, error) {
	f.postgresCalls++
	return []docker.PostgresSize{{Kind: "postgres-database", Name: "app", Bytes: 30}, {Kind: "postgres-table", Name: "public.events", Bytes: 15}}, nil
}

func collectorFixture(t *testing.T) (*Collector, *resourceFixture, *store.Service, *store.Service) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nori.db")
	st, err := store.Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &store.Service{Name: "api", Policy: store.PolicyManual}
	b := &store.Service{Name: "worker", Policy: store.PolicyManual}
	for _, s := range []*store.Service{a, b} {
		if err := st.CreateService(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	f := &resourceFixture{}
	t.Setenv("NORI_BACKUP_DIR", dir)
	return NewCollector(st, f, "unix:///var/run/docker.sock", path), f, a, b
}

func readingsByKey(t *testing.T, rows []store.ResourceReading) map[string]store.ResourceReading {
	t.Helper()
	m := map[string]store.ResourceReading{}
	for _, r := range rows {
		if _, ok := m[r.Key]; ok {
			t.Fatalf("duplicate source %s", r.Key)
		}
		m[r.Key] = r
	}
	return m
}
func mustCollect(t *testing.T, c *Collector, stream string) map[string]store.ResourceReading {
	t.Helper()
	rows, err := c.Collect(t.Context(), stream)
	if err != nil {
		t.Fatal(err)
	}
	return readingsByKey(t, rows)
}

func TestCollectorRuntimeResetsStoppedAndMissing(t *testing.T) {
	c, f, api, worker := collectorFixture(t)
	label := map[string]string{"nori.service": "api"}
	f.containers = []container.Summary{
		{ID: "a", Names: []string{"/api"}, State: "running", Labels: label},
		{ID: "b", State: "exited", Labels: map[string]string{"nori.service-id": fmt.Sprint(api.ID), "nori.service": "old"}},
		{ID: "orphan", State: "exited", Labels: map[string]string{"nori.service-id": "999999", "nori.service": "worker"}}}
	start := time.Now().UTC()
	now := docker.RuntimeStats{At: start.Add(time.Minute), Started: start, Values: map[string]float64{"memory": 0}, Counters: map[string]uint64{"read": 30, "rx": 9, "cpu": 0}}
	f.stats = map[string]docker.RuntimeStats{"a": now}
	rows := mustCollect(t, c, "runtime")
	if rows["container:b"].State != "stopped" || rows["container:a"].State != "running" || len(rows["container:orphan"].Services) != 0 || rows[fmt.Sprintf("missing:%d", worker.ID)].State != "unavailable" {
		t.Fatal(rows)
	}
	if _, ok := rows["container:a"].Values["cpu"]; ok {
		t.Fatal("first sample emitted CPU")
	}
	now.At = now.At.Add(2 * time.Second)
	now.Counters = map[string]uint64{"read": 30, "rx": 7, "cpu": 1000000000}
	f.stats["a"] = now
	values := mustCollect(t, c, "runtime")["container:a"].Values
	if z, ok := values["read"]; !ok || z != 0 || values["cpu"] != 50 {
		t.Fatal(values)
	}
	if _, ok := values["rx"]; ok {
		t.Fatal("counter reset emitted rate", values)
	}
	now.Started = now.At
	now.At = now.At.Add(time.Second)
	now.Counters = map[string]uint64{"read": 9000}
	f.stats["a"] = now
	if _, ok := mustCollect(t, c, "runtime")["container:a"].Values["read"]; ok {
		t.Fatal("restart emitted rate")
	}
	f.containers[0].ID = "replacement"
	f.stats["replacement"] = now
	if _, ok := mustCollect(t, c, "runtime")["container:replacement"].Values["read"]; ok {
		t.Fatal("replacement emitted rate")
	}
}

func TestCollectorStorageOwnershipArchiveAndRemotePath(t *testing.T) {
	c, f, a, b := collectorFixture(t)
	dir := c.archiveDir
	if queued, err := c.st.QueueBackup(t.Context(), a.ID); err != nil || !queued {
		t.Fatal(queued, err)
	}
	jobs, err := c.st.BackupsByStatus(t.Context(), "queued")
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	job := jobs[0]
	if err := c.st.SetBackupStatus(t.Context(), job.ID, "complete", "", 6, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("backup-%d.nori", job.ID)), []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*store.Service{a, b} {
		f.containers = append(f.containers, container.Summary{ID: s.Name, Labels: map[string]string{"nori.service": s.Name}, Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: "shared"}}})
	}
	f.volumes = []*volume.Volume{{Name: "shared", Mountpoint: dir, UsageData: &volume.UsageData{Size: 100}}, {Name: "plugin"}}
	monitorDir := t.TempDir()
	if err := c.st.SetResourceLimits(t.Context(), b.ID, store.ResourceLimits{Directories: []string{monitorDir}}); err != nil {
		t.Fatal(err)
	}
	rows := mustCollect(t, c, "storage")
	if z := rows["volume:shared"]; !slices.Equal(z.Services, []int64{a.ID, b.ID}) {
		t.Fatal(z)
	}
	if c.daemonPaths(t.Context(), f.containers).native && rows["volume:shared"].Values["bytes"] != 94 {
		t.Fatal("archive also counted in backing volume", rows)
	}
	if rows[fmt.Sprintf("backup:%d", a.ID)].Values["bytes"] != 6 || rows["volume:plugin"].State != "unavailable" {
		t.Fatal(rows)
	}
	c.localDaemon = false
	rows = mustCollect(t, c, "storage")
	if rows["directory:"+monitorDir].State != "unavailable" || rows["volume:shared"].Values["bytes"] != 100 || rows[fmt.Sprintf("backup:%d", a.ID)].Values["bytes"] != 6 {
		t.Fatal(rows)
	}
}

func TestCollectorManagedPostgresAndCollision(t *testing.T) {
	c, f, a, b := collectorFixture(t)
	a.DeploymentMode = store.DeploymentModePostgres
	a.TemplateConfig = `{"version":1,"internal_port":80,"restart_policy":"unless-stopped","health":{"command":"true","timeout_seconds":5},"postgres":{"image":"postgres:16","database":"app","user":"app","password_env":"POSTGRES_PASSWORD","connection_url_env":"DATABASE_URL","ready_timeout_seconds":5}}`
	if err := c.st.UpdateService(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	volumeName := fmt.Sprintf("nori-%d-volume-postgres-data", a.ID)
	f.volumes = []*volume.Volume{{Name: volumeName, Labels: map[string]string{"nori.service": "worker"}, UsageData: &volume.UsageData{Size: 10}}}
	rows := mustCollect(t, c, "storage")
	if !slices.Equal(rows["volume:"+volumeName].Services, []int64{b.ID}) || rows["postgres-volume:missing:"+volumeName].State != "unavailable" {
		t.Fatal(rows)
	}
	f.volumes[0].Labels = map[string]string{"nori.template": "1", "nori.service-id": fmt.Sprint(a.ID), "nori.role": "data-volume"}
	f.containers = []container.Summary{{ID: "pg-id", State: "running", Names: []string{fmt.Sprintf("/nori-%d-postgres", a.ID)}, Labels: map[string]string{"nori.template": "1", "nori.service-id": fmt.Sprint(a.ID), "nori.role": "db"}}}
	mustCollect(t, c, "storage")
	if f.postgresCalls != 0 {
		t.Fatal("queried without opt in")
	}
	if err := c.st.SetResourceLimits(t.Context(), a.ID, store.ResourceLimits{PostgresDetails: true}); err != nil {
		t.Fatal(err)
	}
	rows = mustCollect(t, c, "storage")
	if f.postgresCalls != 1 || rows["postgres-volume:"+volumeName].Values["bytes"] != 10 {
		t.Fatal(rows)
	}
	logical := 0
	for _, r := range rows {
		if strings.HasPrefix(r.Kind, "postgres-") && r.Kind != "postgres-volume" {
			if _, ok := r.Values["bytes"]; ok {
				t.Fatal(r)
			}
			logical += int(r.Values["logical"])
		}
	}
	if logical != 45 {
		t.Fatalf("logical sizes: %d", logical)
	}
}

func TestMappedDirectoriesAndRotatedLogs(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"container.log": "abc", "container.log.1.gz": "1234", "container.log.pid": "unrelated"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(root, "container.log"), filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	p := hostPaths{mounts: []container.MountPoint{{Source: "/srv", Destination: root}, {Source: "/other", Destination: filepath.Join(root, "shadow")}}}
	if got, ok := p.toLocal("/srv/container.log"); !ok || got != filepath.Join(root, "container.log") {
		t.Fatal(got, ok)
	}
	if _, ok := p.toLocal("/srv/shadow/elsewhere"); ok {
		t.Fatal("translated through shadowing bind mount")
	}
	if _, ok := p.toLocal(root); ok {
		t.Fatal("confused container path with daemon path")
	}
	if size, err := directoryBytes(t.Context(), root); err != nil || size != 16 {
		t.Fatal(size, err)
	}
	files, size, err := logFiles(t.Context(), filepath.Join(root, "container.log"), "/srv/container.log")
	if err != nil || size != 7 || len(files) != 2 {
		t.Fatal(files, size, err)
	}
	whole := storageSource("directory", "srv", "/srv", nil)
	setBytes(&whole, 16)
	child := storageSource("directory", "logs", "/srv/logs", nil)
	setBytes(&child, 5)
	rows := []store.ResourceReading{whole, child}
	deductContained(rows, append(files, diskRegion{path: "/srv", key: whole.Key}, diskRegion{path: "/srv/logs", key: child.Key}, diskRegion{path: "/srv/logs/archive", bytes: 3}))
	if rows[0].Values["bytes"] != 4 || rows[1].Values["bytes"] != 2 {
		t.Fatal(rows)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := directoryBytes(ctx, root); err == nil {
		t.Fatal("ignored canceled directory scan")
	}
}

func TestDockerFailureKeepsArchiveInventory(t *testing.T) {
	c, f, a, _ := collectorFixture(t)
	f.Err = fmt.Errorf("daemon is unavailable")
	if _, err := c.Collect(t.Context(), "runtime"); err == nil {
		t.Fatal("unavailable runtime inventory counted as success")
	}
	rows := mustCollect(t, c, "storage")
	key := fmt.Sprintf("backup:%d", a.ID)
	if n, ok := rows[key].Values["bytes"]; !ok || n != 0 || rows["inventory:containers"].State != "unavailable" {
		t.Fatal(rows)
	}
}
