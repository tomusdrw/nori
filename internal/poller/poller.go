package poller

import (
	"context"
	"log"
	"sync"
	"time"

	"deploybot/internal/executor"
	"deploybot/internal/store"
)

type Poller struct {
	store    *store.Store
	latest   executor.LatestDigestFunc
	executor *executor.Executor
	interval time.Duration

	mu    sync.RWMutex
	cache map[string]string // service name -> latest digest
}

func New(st *store.Store, latest executor.LatestDigestFunc, ex *executor.Executor, interval time.Duration) *Poller {
	return &Poller{
		store:    st,
		latest:   latest,
		executor: ex,
		interval: interval,
		cache:    make(map[string]string),
	}
}

func (p *Poller) CachedDigest(serviceName string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cache[serviceName]
}

func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	p.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Tick(ctx)
		}
	}
}

func (p *Poller) Tick(ctx context.Context) {
	svcs, err := p.store.ListServices(ctx)
	if err != nil {
		log.Printf("poller: list services: %v", err)
		return
	}
	for _, svc := range svcs {
		digest, err := p.latest(ctx, svc.WatchedImage)
		if err != nil {
			log.Printf("poller: digest %s: %v", svc.WatchedImage, err)
			continue
		}
		p.mu.Lock()
		prev := p.cache[svc.Name]
		p.cache[svc.Name] = digest
		p.mu.Unlock()

		if svc.Policy == store.PolicyImmediate && prev != "" && prev != digest {
			log.Printf("poller: digest changed for %s, triggering auto-deploy", svc.Name)
			if _, err := p.executor.Deploy(ctx, svc.ID, store.TriggerAuto); err != nil {
				log.Printf("poller: auto-deploy %s: %v", svc.Name, err)
			}
		}
	}
}
