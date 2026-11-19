package resources

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"

	"nori/internal/deploytemplate"
	"nori/internal/store"
)

func storageSource(kind, key, name string, owners []int64) store.ResourceReading {
	return store.ResourceReading{Key: kind + ":" + key, Kind: kind, Name: name, Services: owners, State: "unavailable"}
}

func setBytes(r *store.ResourceReading, bytes int64) {
	if bytes >= 0 {
		r.State = "available"
		r.Values = map[string]float64{"bytes": float64(bytes)}
	}
}

type ownership map[string][]int64

func (m ownership) add(key string, ids ...int64) {
	all := append(m[key], ids...)
	slices.Sort(all)
	m[key] = slices.Compact(all)
}

type storageSelection struct {
	volumes, dirs, binds ownership
	expected             map[string]int64
	dbVolumes            map[string]bool
}

func (c *Collector) storage(ctx context.Context, containers []container.Summary, cat catalog, inventoryErr error) []store.ResourceReading {
	selected, rows := c.selectStorage(ctx, containers, cat)
	if inventoryErr != nil {
		rows = append(rows, storageSource("inventory", "containers", "Docker container inventory", nil))
	}
	paths := c.daemonPaths(ctx, containers)
	volumes, regions := c.volumeReadings(ctx, selected, cat)
	rows = append(rows, volumes...)
	for _, r := range directoryReadings(ctx, selected.dirs, selected.binds, paths) {
		rows = append(rows, r)
		regions = append(regions, diskRegion{path: r.Name, key: r.Key})
	}
	layers, logs := c.containerStorage(ctx, containers, cat, paths)
	rows = append(rows, layers...)
	regions = append(regions, logs...)
	archives, files := c.archives(ctx, cat, paths)
	rows = append(rows, archives...)
	regions = append(regions, files...)
	deductContained(rows, regions)
	return rows
}

func (c *Collector) selectStorage(ctx context.Context, containers []container.Summary, cat catalog) (storageSelection, []store.ResourceReading) {
	selected := storageSelection{volumes: ownership{}, dirs: ownership{}, binds: ownership{},
		expected: map[string]int64{}, dbVolumes: map[string]bool{}}
	var out []store.ResourceReading
	databases := map[int64]string{}
	for _, ctr := range containers {
		owners := cat.owners(ctr.Labels)
		for _, m := range ctr.Mounts {
			switch m.Type {
			case mount.TypeVolume:
				selected.volumes.add(m.Name, owners...)
			case mount.TypeBind:
				selected.binds.add(filepath.Clean(m.Source), owners...)
			}
		}
		if len(owners) == 1 && ctr.State == "running" && ctr.Labels["nori.template"] == "1" && ctr.Labels["nori.role"] == "db" {
			id := owners[0]
			if ctr.Labels["nori.service-id"] == strconv.FormatInt(id, 10) && containerName(ctr) == fmt.Sprintf("nori-%d-postgres", id) {
				databases[id] = ctr.ID
			}
		}
	}
	settings, err := c.st.ActiveResourceLimits(ctx)
	if err != nil {
		out = append(out, storageSource("inventory", "directories", "Selected monitoring directories", nil))
	}
	for _, id := range slices.Sorted(maps.Keys(cat.byID)) {
		s := cat.byID[id]
		limits := settings[id]
		for _, dir := range limits.Directories {
			selected.dirs.add(dir, id)
		}
		if s.DeploymentMode == store.DeploymentModePostgres || s.DeploymentMode == store.DeploymentModeSingleContainer {
			config, err := deploytemplate.ParseConfig(deploytemplate.Mode(s.DeploymentMode), s.TemplateConfig)
			if err != nil {
				out = append(out, storageSource("inventory", fmt.Sprintf("template:%d", id), "Managed resource configuration", []int64{id}))
			} else {
				for _, m := range config.Volumes {
					selected.expected[fmt.Sprintf("nori-%d-volume-%s", id, m.Name)] = id
				}
				if config.Postgres != nil {
					name := fmt.Sprintf("nori-%d-volume-postgres-data", id)
					selected.dbVolumes[name] = true
					selected.expected[name] = id
					if limits.PostgresDetails {
						out = append(out, c.postgres(ctx, id, databases[id], *config.Postgres)...)
					}
				}
			}
		}
		policy, _, err := c.st.BackupPolicy(ctx, id)
		if err != nil {
			out = append(out, storageSource("inventory", fmt.Sprintf("sources:%d", id), "Selected storage sources", []int64{id}))
			continue
		}
		for _, src := range policy.Sources {
			switch src.Type {
			case "bind":
				if filepath.IsAbs(src.Path) && src.Path != "/" && filepath.Clean(src.Path) == src.Path {
					selected.dirs.add(src.Path, id)
				}
			case "volume":
				selected.volumes.add(src.Path, id)
			}
		}
	}
	return selected, out
}

func (c *Collector) volumeReadings(ctx context.Context, selected storageSelection, cat catalog) ([]store.ResourceReading, []diskRegion) {
	var out []store.ResourceReading
	var vs []*volume.Volume
	if c.dk != nil {
		call, cancel := context.WithTimeout(ctx, 25*time.Second)
		var err error
		vs, err = c.dk.ResourceVolumes(call)
		cancel()
		if err != nil {
			out = append(out, storageSource("inventory", "volumes", "Docker volume inventory", nil))
		}
	} else {
		out = append(out, storageSource("inventory", "volumes", "Docker volume inventory", nil))
	}
	inventory := map[string]*volume.Volume{}
	for _, v := range vs {
		if v == nil || v.Name == "" {
			continue
		}
		inventory[v.Name] = v
		selected.volumes.add(v.Name, cat.owners(v.Labels)...)
	}

	for name, id := range selected.expected {
		v := inventory[name]
		if v == nil {
			selected.volumes.add(name, id)
			continue
		}
		if v.Labels["nori.template"] == "1" && v.Labels["nori.role"] == "data-volume" && v.Labels["nori.service-id"] == strconv.FormatInt(id, 10) {
			continue
		}
		kind := "volume"
		if selected.dbVolumes[name] {
			kind = "postgres-volume"
		}
		out = append(out, storageSource(kind, "missing:"+name, name+" (ownership mismatch)", []int64{id}))
		delete(selected.dbVolumes, name)
	}

	// Backing paths allow archive bytes to be charged separately from the volume
	// or selected directory that contains them.
	var regions []diskRegion
	for _, name := range slices.Sorted(maps.Keys(selected.volumes)) {
		kind := "volume"
		if selected.dbVolumes[name] {
			kind = "postgres-volume"
		}
		r := storageSource(kind, name, name, selected.volumes[name])
		if v := inventory[name]; v != nil {
			if v.UsageData != nil {
				setBytes(&r, v.UsageData.Size)
			}
			if filepath.IsAbs(v.Mountpoint) {
				regions = append(regions, diskRegion{path: v.Mountpoint, key: r.Key})
				if _, ok := selected.dirs[v.Mountpoint]; ok {
					selected.dirs.add(v.Mountpoint, r.Services...)
					selected.dirs.add(v.Mountpoint, selected.binds[v.Mountpoint]...)
					r.Services = selected.dirs[v.Mountpoint]
					delete(selected.dirs, v.Mountpoint)
				}
			}
		}
		out = append(out, r)
	}
	return out, regions
}

func directoryReadings(ctx context.Context, dirs, binds ownership, paths hostPaths) []store.ResourceReading {
	for dir := range dirs {
		// A service mounting an ancestor can access all selected subdirectories.
		for p := dir; ; p = filepath.Dir(p) {
			dirs.add(dir, binds[p]...)
			if p != dir {
				dirs.add(dir, dirs[p]...)
			}
			if p == "/" || p == "." {
				break
			}
		}
	}
	return measureAll(ctx, slices.Sorted(maps.Keys(dirs)), func(ctx context.Context, dir string) store.ResourceReading {
		r := storageSource("directory", dir, dir, dirs[dir])
		if local, ok := paths.toLocal(dir); ok {
			call, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			if size, err := directoryBytes(call, local); err == nil {
				setBytes(&r, size)
			}
		}
		return r
	})
}

func (c *Collector) containerStorage(ctx context.Context, containers []container.Summary, cat catalog, paths hostPaths) ([]store.ResourceReading, []diskRegion) {
	type diskPair struct {
		writable, logs store.ResourceReading
		files          []diskRegion
	}
	pairs := measureAll(ctx, containers, func(ctx context.Context, ctr container.Summary) diskPair {
		owners := cat.owners(ctr.Labels)
		name := containerName(ctr)
		p := diskPair{writable: storageSource("writable", ctr.ID, name, owners), logs: storageSource("logs", ctr.ID, name, owners)}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s, err := c.dk.ResourceStorage(call, ctr.ID)
		if err != nil {
			return p
		}
		if s.Writable != nil {
			setBytes(&p.writable, *s.Writable)
		}
		if s.LogDriver == "none" {
			setBytes(&p.logs, 0)
			return p
		}
		if s.LogDriver == "json-file" || s.LogDriver == "local" {
			if local, ok := paths.toLocal(s.LogPath); ok {
				if files, size, err := logFiles(call, local, s.LogPath); err == nil {
					p.files = files
					setBytes(&p.logs, size)
				}
			}
		}
		return p
	})
	var out []store.ResourceReading
	var regions []diskRegion
	for _, p := range pairs {
		out = append(out, p.writable, p.logs)
		regions = append(regions, p.files...)
	}
	return out, regions
}

func (c *Collector) postgres(ctx context.Context, id int64, containerID string, cfg deploytemplate.PostgresConfig) []store.ResourceReading {
	base := storageSource("postgres-database", strconv.FormatInt(id, 10), cfg.Database, []int64{id})
	if containerID == "" || c.dk == nil {
		return []store.ResourceReading{base}
	}
	call, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	rows, err := c.dk.PostgresSizes(call, containerID, cfg.User, cfg.Database)
	if err != nil {
		return []store.ResourceReading{base}
	}
	var readings []store.ResourceReading
	for _, row := range rows {
		r := base
		if row.Kind == "postgres-table" {
			r = storageSource(row.Kind, fmt.Sprintf("%d:%s", id, row.Name), row.Name, []int64{id})
		}
		r.State = "available"
		r.Values = map[string]float64{"logical": float64(row.Bytes)}
		readings = append(readings, r)
	}
	return readings
}

func (c *Collector) archives(ctx context.Context, cat catalog, paths hostPaths) ([]store.ResourceReading, []diskRegion) {
	jobs, err := c.st.BackupsByStatus(ctx, "complete")
	if err != nil {
		return []store.ResourceReading{storageSource("inventory", "archives", "Local backup inventory", nil)}, nil
	}
	byService := map[int64]*store.ResourceReading{}
	add := func(id int64, name string, owners []int64) *store.ResourceReading {
		if r := byService[id]; r != nil {
			return r
		}
		r := storageSource("backup", strconv.FormatInt(id, 10), name+" backups", owners)
		setBytes(&r, 0)
		byService[id] = &r
		return &r
	}
	for _, s := range cat.byID {
		add(s.ID, s.Name, []int64{s.ID})
	}
	var files []diskRegion
	for _, job := range jobs {
		r := add(job.ServiceID, job.ServiceName, nil)
		path := filepath.Join(c.archiveDir, fmt.Sprintf("backup-%d.nori", job.ID))
		f, err := os.Lstat(path)
		if err != nil || c.archiveDir == "" || !f.Mode().IsRegular() {
			r.State = "unavailable"
			r.Values = nil
			continue
		}
		if r.State == "available" {
			r.Values["bytes"] += float64(f.Size())
		}
		if host, ok := paths.toHost(path); ok {
			files = append(files, diskRegion{path: host, bytes: f.Size()})
		} else if paths.selfID != "" && !paths.isMounted(path) {
			files = append(files, diskRegion{parentKey: "writable:" + paths.selfID, bytes: f.Size()})
		}
	}
	var out []store.ResourceReading
	for _, id := range slices.Sorted(maps.Keys(byService)) {
		out = append(out, *byService[id])
	}
	return out, files
}
