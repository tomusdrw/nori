package poller

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"nori/internal/executor"
	"nori/internal/store"
)

type Poller struct {
	store    *store.Store
	latest   executor.LatestDigestFunc
	executor *executor.Executor
	interval time.Duration

	mu      sync.RWMutex
	pending map[int64]string
	cache   map[string]string // service name -> latest digest
}

func New(st *store.Store, latest executor.LatestDigestFunc, ex *executor.Executor, interval time.Duration) *Poller {
	return &Poller{
		store:    st,
		latest:   latest,
		executor: ex,
		interval: interval,
		pending:  make(map[int64]string),
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
		if svc.DeploymentMode == store.DeploymentModeManualScript {
			p.mu.Lock()
			delete(p.cache, svc.Name)
			delete(p.pending, svc.ID)
			p.mu.Unlock()
			continue
		}
		digest, err := p.latest(ctx, svc.WatchedImage)
		if err != nil {
			log.Printf("poller: digest %s: %v", svc.WatchedImage, err)
			continue
		}
		p.mu.Lock()
		prev := p.cache[svc.Name]
		p.cache[svc.Name] = digest
		if svc.Policy != store.PolicyImmediate {
			delete(p.pending, svc.ID)
		} else if prev != "" && prev != digest {
			p.pending[svc.ID] = digest
		}
		pending := p.pending[svc.ID]
		p.mu.Unlock()

		if pending != "" {
			_, err := p.executor.Deploy(ctx, svc.ID, store.TriggerAuto)
			if err != nil {
				log.Printf("poller: auto-deploy %s: %v", svc.Name, err)
			}
			if !errors.Is(err, executor.ErrServiceBusy) {
				p.mu.Lock()
				if p.pending[svc.ID] == pending {
					delete(p.pending, svc.ID)
				}
				p.mu.Unlock()
			}
		}
	}
}
