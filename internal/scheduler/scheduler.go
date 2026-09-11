package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/robfig/cron/v3"

	"deploybot/internal/executor"
	"deploybot/internal/store"
)

type Scheduler struct {
	cron     *cron.Cron
	store    *store.Store
	executor *executor.Executor
	entries  map[int64]scheduledEntry
	cancel   context.CancelFunc
	done     chan struct{}
}

type scheduledEntry struct {
	id   cron.EntryID
	expr string
}

func New(st *store.Store, ex *executor.Executor) *Scheduler {
	return &Scheduler{
		cron:     cron.New(),
		store:    st,
		executor: ex,
		entries:  make(map[int64]scheduledEntry),
	}
}

func (s *Scheduler) Start(ctx context.Context) error {
	ctx, s.cancel = context.WithCancel(ctx)
	if err := s.reload(ctx); err != nil {
		s.cancel()
		return err
	}
	s.cron.Start()
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.reload(ctx); err != nil {
					log.Printf("scheduler: reload: %v", err)
				}
			}
		}
	}()
	return nil
}

// Reload preserves unchanged entries (including @every schedules) and observes
// creations, edits and removals from either the dashboard or MCP without restart.
func (s *Scheduler) reload(ctx context.Context) error {
	svcs, err := s.store.ListServiceSchedules(ctx)
	if err != nil {
		return err
	}
	wanted := make(map[int64]string)
	for _, svc := range svcs {
		wanted[svc.ID] = svc.CronExpr
	}
	for id, entry := range s.entries {
		if wanted[id] != entry.expr {
			s.cron.Remove(entry.id)
			delete(s.entries, id)
		}
	}
	for _, svc := range svcs {
		if _, ok := s.entries[svc.ID]; ok {
			continue
		}
		svcID := svc.ID
		svcName := svc.Name
		expr := svc.CronExpr
		id, err := s.cron.AddFunc(expr, func() {
			current, err := s.store.GetService(ctx, svcID)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, context.Canceled) {
					log.Printf("scheduler: load service %q (id %d): %v", svcName, svcID, err)
				}
				return
			}
			if current.Policy != store.PolicyScheduled || current.CronExpr != expr {
				return
			}
			log.Printf("scheduler: triggering deploy for %q", svcName)
			if _, err := s.executor.Deploy(ctx, svcID, store.TriggerScheduled); err != nil {
				log.Printf("scheduler: deploy %q: %v", svcName, err)
			}
		})
		if err != nil {
			log.Printf("scheduler: bad cron %q for %q: %v", expr, svcName, err)
			// Remember invalid legacy UI configurations too, so the live reload
			// reports them once per change instead of once every second.
			s.entries[svcID] = scheduledEntry{expr: expr}
			continue
		}
		s.entries[svcID] = scheduledEntry{id: id, expr: expr}
	}
	return nil
}

func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
		if s.done != nil {
			<-s.done
		}
	}
	s.cron.Stop()
}
