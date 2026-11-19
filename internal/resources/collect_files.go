package resources

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"

	"nori/internal/store"
)

type hostPaths struct {
	native bool
	selfID string
	mounts []container.MountPoint
}

func (c *Collector) daemonPaths(ctx context.Context, containers []container.Summary) hostPaths {
	if !c.localDaemon || c.dk == nil {
		return hostPaths{}
	}
	hostname, _ := os.Hostname()
	for _, c := range containers {
		if len(hostname) >= 12 && strings.HasPrefix(c.ID, hostname) {
			return hostPaths{selfID: c.ID, mounts: c.Mounts}
		}
	}
	// A Unix socket mounted into Nori still names the daemon's filesystem.
	// Without verified mounts never resolve daemon paths in a container's root.
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv", "/run/systemd/container"} {
		if _, err := os.Stat(marker); err == nil {
			return hostPaths{}
		}
	}
	if os.Getenv("container") != "" {
		return hostPaths{}
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		for _, tag := range []string{"docker", "containerd", "kubepods", "libpod", "lxc"} {
			if strings.Contains(string(data), tag) {
				return hostPaths{}
			}
		}
	}
	// Docker Desktop and tunnels can expose a Unix socket backed by another
	// machine. Confirm the native host identity before touching daemon paths.
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	daemon, err := c.dk.ResourceHostName(call)
	return hostPaths{native: err == nil && hostname != "" && hostname == daemon}
}

func within(root, path string) bool {
	return root != "" && (root == path || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/"))
}

func (p hostPaths) translate(path string, reverse bool) (string, bool) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false
	}
	if p.native {
		return path, true
	}
	longest, mapped := "", ""
	for _, m := range p.mounts {
		from, to := m.Source, m.Destination
		if reverse {
			from, to = to, from
		}
		if len(from) <= len(longest) || !within(from, path) {
			continue
		}
		longest = from
		mapped = ""
		if filepath.IsAbs(to) {
			rel, _ := filepath.Rel(from, path)
			mapped = filepath.Join(to, rel)
		}
	}
	return mapped, mapped != ""
}

func (p hostPaths) toLocal(host string) (string, bool) {
	mapped, ok := p.translate(host, false)
	if !ok {
		return "", false
	}
	// An overlapping mount in Nori can shadow part of a mapped host tree.
	roundtrip, ok := p.toHost(mapped)
	if !ok || roundtrip != host {
		return "", false
	}
	// A directory scan would traverse any unrelated mounts below this path.
	for _, m := range p.mounts {
		if m.Destination != mapped && within(mapped, m.Destination) {
			rel, _ := filepath.Rel(mapped, m.Destination)
			if m.Source != filepath.Join(host, rel) {
				return "", false
			}
		}
	}
	return mapped, true
}
func (p hostPaths) toHost(path string) (string, bool) { return p.translate(path, true) }

func (p hostPaths) isMounted(path string) bool {
	for _, m := range p.mounts {
		if within(m.Destination, path) {
			return true
		}
	}
	return false
}

func directoryBytes(ctx context.Context, path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, errors.New("not a directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	type inode struct{ dev, ino uint64 }
	hardlinks := map[inode]bool{}
	var total int64
	entries := 0
	// Stream directory entries in batches. A tree with millions of filenames
	// should be marked unmeasurable within budget, without sorting it in memory.
	var visit func(string, int) error
	visit = func(name string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 128 {
			return errors.New("directory depth limit reached")
		}
		dir, err := root.Open(name)
		if err != nil {
			return err
		}
		defer dir.Close()
		for {
			list, err := dir.ReadDir(128)
			if err != nil && err != io.EOF {
				return err
			}
			for _, e := range list {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries++
				if entries > 500000 {
					return errors.New("directory entry limit reached")
				}
				child := filepath.Join(name, e.Name())
				if e.IsDir() {
					if err := visit(child, depth+1); err != nil {
						return err
					}
					continue
				}
				if !e.Type().IsRegular() {
					continue
				}
				data, err := root.Lstat(child)
				if err != nil {
					return err
				}
				if !data.Mode().IsRegular() {
					continue
				}
				if st, ok := data.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
					key := inode{uint64(st.Dev), st.Ino}
					if hardlinks[key] {
						continue
					}
					hardlinks[key] = true
				}
				total += data.Size()
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	err = visit(".", 0)
	return total, err
}

// diskRegion represents a measured directory (key) or file (bytes). Inventory
// sizes are inclusive; remove children once from their nearest known parent.
type diskRegion struct {
	path, key, parentKey string
	bytes                int64
}

func deductContained(rows []store.ResourceReading, regions []diskRegion) {
	parents := map[string]string{}
	inclusive := map[string]float64{}
	indices := map[string]int{}
	for i, r := range rows {
		if n, ok := r.Values["bytes"]; ok {
			inclusive[r.Key] = n
			indices[r.Key] = i
		}
	}
	for _, r := range regions {
		if _, ok := inclusive[r.key]; ok && r.path != "" {
			parents[r.path] = r.key
		}
	}
	for _, r := range regions {
		n := float64(r.bytes)
		if r.key != "" {
			var ok bool
			n, ok = inclusive[r.key]
			if !ok {
				continue
			}
		}
		parent := r.parentKey
		if parent == "" && r.path != "" {
			for path := filepath.Dir(r.path); ; path = filepath.Dir(path) {
				if key := parents[path]; key != "" {
					parent = key
					break
				}
				if path == "/" || path == "." {
					break
				}
			}
		}
		if i, ok := indices[parent]; ok {
			rows[i].Values["bytes"] = max(0, rows[i].Values["bytes"]-n)
		}
	}
}

func logFiles(ctx context.Context, local, host string) ([]diskRegion, int64, error) {
	// Docker's json-file and local drivers rotate the base file as .1, .2, ...
	// optionally adding .gz. Ignore other files in the container's directory.
	base := filepath.Base(local)
	dir, err := os.Open(filepath.Dir(local))
	if err != nil {
		return nil, 0, err
	}
	defer dir.Close()
	var files []diskRegion
	var total int64
	exists := false
	for {
		batch, err := dir.ReadDir(128)
		if err != nil && err != io.EOF {
			return nil, 0, err
		}
		for _, f := range batch {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			name := f.Name()
			if name != base {
				suffix, ok := strings.CutPrefix(name, base+".")
				if !ok {
					continue
				}
				if _, err := strconv.ParseUint(strings.TrimSuffix(suffix, ".gz"), 10, 32); err != nil {
					continue
				}
			}
			info, err := f.Info()
			if err != nil {
				return nil, 0, err
			}
			if !info.Mode().IsRegular() {
				return nil, 0, errors.New("unexpected log file type")
			}
			exists = exists || name == base
			total += info.Size()
			files = append(files, diskRegion{path: filepath.Join(filepath.Dir(host), name), bytes: info.Size()})
		}
		if err == io.EOF {
			break
		}
	}
	if !exists {
		return nil, 0, fs.ErrNotExist
	}
	return files, total, nil
}
