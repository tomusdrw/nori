package resources

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"

	"nori/internal/backup"
	"nori/internal/docker"
	"nori/internal/store"
)

type Collector struct {
	st          *store.Store
	dk          docker.ResourceClient
	previous    map[string]docker.RuntimeStats
	runtimeMu   sync.Mutex
	localDaemon bool
	archiveDir  string
}

func NewCollector(st *store.Store, dk docker.Client, dockerHost, dbPath string) *Collector {
	client, _ := dk.(docker.ResourceClient)
	if dockerHost == "" {
		dockerHost = os.Getenv("DOCKER_HOST")
	}
	dir, _ := backup.ArchiveDir(dbPath)
	return &Collector{st: st, dk: client, localDaemon: dockerHost == "" || strings.HasPrefix(dockerHost, "unix://"), archiveDir: dir}
}

func (c *Collector) Collect(ctx context.Context, stream string) ([]store.ResourceReading, error) {
	svcs, err := c.st.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	catalog := catalogServices(svcs)
	var containers []container.Summary
	if c.dk == nil {
		err = errors.New("Docker resource inventory unavailable")
	} else {
		call, cancel := context.WithTimeout(ctx, 8*time.Second)
		containers, err = c.dk.ResourceContainers(call)
		cancel()
	}
	switch stream {
	case "runtime":
		if err != nil {
			return nil, err
		}
		return c.runtime(ctx, containers, catalog), ctx.Err()
	case "storage":
		return c.storage(ctx, containers, catalog, err), ctx.Err()
	default:
		return nil, fmt.Errorf("unknown resource stream %q", stream)
	}
}

type catalog struct {
	byID   map[int64]*store.Service
	byName map[string]int64
}

func catalogServices(services []*store.Service) catalog {
	c := catalog{byID: map[int64]*store.Service{}, byName: map[string]int64{}}
	for _, s := range services {
		c.byID[s.ID] = s
		c.byName[s.Name] = s.ID
	}
	return c
}
func (c catalog) owners(labels map[string]string) []int64 {
	if raw, ok := labels["nori.service-id"]; ok {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && c.byID[id] != nil {
			return []int64{id}
		}
		return nil // A stale ID must not transfer ownership to a new service with the old name.
	}
	if id := c.byName[labels[docker.ServiceLabel]]; id != 0 {
		return []int64{id}
	}
	return nil
}

func containerName(item container.Summary) string {
	if len(item.Names) > 0 {
		return strings.TrimPrefix(item.Names[0], "/")
	}
	return item.ID
}

// Each worker owns its result slot. The limit also bounds concurrent filesystem scans
// and Docker requests on the independently scheduled storage and runtime streams.
func measureAll[T, R any](ctx context.Context, items []T, measure func(context.Context, T) R) []R {
	result := make([]R, len(items))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(len(items), 4) {
		wg.Go(func() {
			for i := range jobs {
				result[i] = measure(ctx, items[i])
			}
		})
	}
	for i := range items {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return result
}

func (c *Collector) runtime(ctx context.Context, items []container.Summary, services catalog) []store.ResourceReading {
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	type sample struct {
		reading store.ResourceReading
		stats   docker.RuntimeStats
	}
	measured := measureAll(ctx, items, func(ctx context.Context, item container.Summary) sample {
		r := store.ResourceReading{Key: "container:" + item.ID, Name: containerName(item), Kind: "container", State: "stopped", Services: services.owners(item.Labels)}
		switch item.State {
		case "exited", "dead", "created":
			return sample{reading: r}
		case "running":
		default:
			r.State = "unavailable"
			return sample{reading: r}
		}
		call, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		now, err := c.dk.ResourceStats(call, item.ID)
		if err != nil {
			r.State = "unavailable"
			return sample{reading: r}
		}
		r.State = "running"
		r.Values = runtimeValues(c.previous[item.ID], now)
		return sample{reading: r, stats: now}
	})
	out := make([]store.ResourceReading, 0, len(measured))
	next := map[string]docker.RuntimeStats{}
	seen := map[int64]bool{}
	for i, m := range measured {
		out = append(out, m.reading)
		for _, id := range m.reading.Services {
			seen[id] = true
		}
		if !m.stats.At.IsZero() {
			next[items[i].ID] = m.stats
		}
	}
	c.previous = next
	for _, id := range slices.Sorted(maps.Keys(services.byID)) {
		if !seen[id] {
			out = append(out, store.ResourceReading{Key: fmt.Sprintf("missing:%d", id), Name: services.byID[id].Name, Kind: "container", State: "unavailable", Services: []int64{id}})
		}
	}
	return out
}

func runtimeValues(before, now docker.RuntimeStats) map[string]float64 {
	values := maps.Clone(now.Values)
	if values == nil {
		values = map[string]float64{}
	}
	elapsed := now.At.Sub(before.At).Seconds()
	if before.At.IsZero() || elapsed <= 0 || !before.Started.Equal(now.Started) {
		return values
	}
	for key, count := range now.Counters {
		old, ok := before.Counters[key]
		if !ok || old > count {
			continue
		}
		rate := float64(count-old) / elapsed
		if key == "cpu" {
			rate /= 1e7
		} // nanoseconds of CPU time per second, as a percent of one core
		values[key] = rate
	}
	return values
}
