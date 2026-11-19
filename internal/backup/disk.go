package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"nori/internal/store"
)

var ErrDiskSpace = errors.New("backup disk is below its free-space reserve")

const diskSetting = "backup_disk"
const GiB int64 = 1 << 30

type DiskPolicy struct {
	WarningPercent  int64
	WarningBytes    int64
	CriticalPercent int64
	CriticalBytes   int64
	Paths           []string
}

func defaultDiskPolicy() DiskPolicy {
	return DiskPolicy{WarningPercent: 15, WarningBytes: 5 * GiB, CriticalPercent: 5, CriticalBytes: GiB}
}

type DiskUsage struct {
	Path      string
	Available int64
	Total     int64
	Status    string
}

func (m *Manager) DiskPolicy(ctx context.Context) (DiskPolicy, error) {
	p := defaultDiskPolicy()
	raw, err := m.store.GetSetting(ctx, diskSetting)
	if errors.Is(err, store.ErrNotFound) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal([]byte(raw), &p)
}

func (m *Manager) SaveDiskPolicy(ctx context.Context, p DiskPolicy) error {
	if p.CriticalPercent < 0 || p.WarningPercent < p.CriticalPercent || p.WarningPercent > 95 ||
		p.CriticalBytes < 0 || p.WarningBytes < p.CriticalBytes || len(p.Paths) > 32 {
		return errors.New("invalid disk thresholds or too many monitor paths")
	}
	for _, path := range p.Paths {
		if !filepath.IsAbs(path) {
			return errors.New("additional disk paths must be absolute")
		}
		if _, _, err := diskUsage(path, p); err != nil {
			return fmt.Errorf("cannot measure disk path %s", path)
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return m.store.SetSetting(ctx, diskSetting, string(raw))
}

func diskUsage(path string, p DiskPolicy) (DiskUsage, string, error) {
	var stat unix.Statfs_t
	var inode unix.Stat_t
	u := DiskUsage{Path: path, Status: "unavailable"}
	if err := unix.Statfs(path, &stat); err != nil {
		return u, "", err
	}
	if err := unix.Stat(path, &inode); err != nil {
		return u, "", err
	}
	u.Available = int64(stat.Bavail) * int64(stat.Bsize)
	u.Total = int64(stat.Blocks) * int64(stat.Bsize)
	u.Status = diskLevel(u.Available, u.Total, p)
	return u, fmt.Sprint(inode.Dev), nil
}

func diskLevel(available, total int64, p DiskPolicy) string {
	if available < p.CriticalBytes || available < total/100*p.CriticalPercent {
		return "critical"
	}
	if available < p.WarningBytes || available < total/100*p.WarningPercent {
		return "warning"
	}
	return "healthy"
}

func (m *Manager) Disks(ctx context.Context) ([]DiskUsage, error) {
	p, err := m.DiskPolicy(ctx)
	if err != nil {
		return nil, err
	}
	paths := append([]string{m.dir, m.dataDir}, p.Paths...)
	seen := make(map[string]bool)
	var results []DiskUsage
	for _, path := range paths {
		u, device, err := diskUsage(path, p)
		if err != nil || !seen[device] {
			results = append(results, u)
		}
		seen[device] = true
	}
	return results, nil
}

func (m *Manager) diskGuard(ctx context.Context) (func() error, error) {
	p, err := m.DiskPolicy(ctx)
	if err != nil {
		return nil, err
	}
	var next time.Time
	return func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().Before(next) {
			return nil
		}
		for _, path := range append([]string{m.dir, m.dataDir}, p.Paths...) {
			u, _, err := diskUsage(path, p)
			if err != nil {
				return err
			}
			if u.Status == "critical" {
				return ErrDiskSpace
			}
		}
		next = time.Now().Add(250 * time.Millisecond)
		return nil
	}, nil
}

func (m *Manager) monitorDisks(ctx context.Context) {
	disks, err := m.Disks(ctx)
	if err != nil {
		log.Printf("backup: measure disks: %v", err)
		return
	}
	for _, disk := range disks {
		key := "backup_disk_alert:" + disk.Path
		raw, err := m.store.GetSetting(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			continue
		}
		state, ts, _ := strings.Cut(raw, ":")
		last, _ := time.Parse(time.RFC3339, ts)
		if disk.Status == "healthy" && (state == "" || state == "healthy") {
			continue
		}
		if state == disk.Status && time.Since(last) < 24*time.Hour {
			continue
		}
		message := fmt.Sprintf("Nori backup disk %s: %s (%.1f GiB available of %.1f GiB)", disk.Path, disk.Status, float64(disk.Available)/float64(GiB), float64(disk.Total)/float64(GiB))
		if err := m.notify(ctx, message); err != nil {
			log.Printf("backup: disk alert delivery: %v", err)
			continue
		}
		if err := m.store.SetSetting(ctx, key, disk.Status+":"+time.Now().UTC().Format(time.RFC3339)); err != nil {
			log.Printf("backup: save disk alert: %v", err)
		}
		log.Print(message)
	}
}

func backupDir(dataPath string) (dir, dataDir string, err error) {
	dataDir, err = filepath.Abs(filepath.Dir(dataPath))
	if err != nil {
		return "", "", err
	}
	dir = os.Getenv("NORI_BACKUP_DIR")
	if dir == "" {
		dir = filepath.Join(dataDir, "backups")
	}
	dir, err = filepath.Abs(dir)
	return
}

// watchDisk cancels an external command when its writes put the host below
// the configured reserve, including commands that write directly to Docker.
func watchDisk(ctx context.Context, check func() error, run func(context.Context) error) error {
	if err := check(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return errors.Join(err, check())
		case <-ticker.C:
			if err := check(); err != nil {
				cancel()
				<-done
				return err
			}
		}
	}
}
