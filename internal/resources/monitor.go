// Package resources samples runtime and storage independently of deployments.
package resources

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"nori/internal/store"
)

const (
	RuntimeInterval = time.Minute
	StorageInterval = 15 * time.Minute
)

type collector interface {
	Collect(context.Context, string) ([]store.ResourceReading, error)
}

type Monitor struct {
	store  *store.Store
	source collector
	notify func(ctx context.Context, message, path string) error
}

func NewMonitor(st *store.Store, src collector, notify func(context.Context, string, string) error) *Monitor {
	return &Monitor{store: st, source: src, notify: notify}
}

func (m *Monitor) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for stream, interval := range map[string]time.Duration{"runtime": RuntimeInterval, "storage": StorageInterval} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for ctx.Err() == nil {
				if err := m.sample(ctx, stream, interval); err != nil && ctx.Err() == nil {
					log.Printf("resources: %s: %v", stream, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	workers.Wait()
}

func (m *Monitor) sample(ctx context.Context, stream string, interval time.Duration) error {
	at := time.Now().UTC()
	readCtx, cancel := context.WithTimeout(ctx, interval*3/4)
	readings, err := m.source.Collect(readCtx, stream)
	cancel()
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	snap := store.ResourceSnapshot{At: at, Readings: readings}
	if err := m.store.SaveResourceSnapshot(ctx, stream, snap); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if m.notify == nil {
		return nil
	}
	alertCtx, cancelAlerts := context.WithTimeout(ctx, interval/4)
	defer cancelAlerts()
	if stream == "storage" {
		return m.checkAlerts(alertCtx, snap)
	}
	return m.checkRuntimeAlerts(alertCtx, snap)
}
