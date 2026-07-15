package scheduler

import (
	"context"
	"log"

	"github.com/robfig/cron/v3"

	"deploybot/internal/executor"
	"deploybot/internal/store"
)

type Scheduler struct {
	cron     *cron.Cron
	store    *store.Store
	executor *executor.Executor
}

func New(st *store.Store, ex *executor.Executor) *Scheduler {
	return &Scheduler{
		cron:     cron.New(),
		store:    st,
		executor: ex,
	}
}

func (s *Scheduler) Start(ctx context.Context) error {
	svcs, err := s.store.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		if svc.Policy != store.PolicyScheduled || svc.CronExpr == "" {
			continue
		}
		svcID := svc.ID
		expr := svc.CronExpr
		_, err := s.cron.AddFunc(expr, func() {
			if _, err := s.executor.Deploy(ctx, svcID, store.TriggerScheduled); err != nil {
				log.Printf("scheduler: deploy service %d: %v", svcID, err)
			}
		})
		if err != nil {
			log.Printf("scheduler: bad cron %q for service %d: %v", expr, svcID, err)
		}
	}
	s.cron.Start()
	return nil
}

func (s *Scheduler) Stop() {
	s.cron.Stop()
}
