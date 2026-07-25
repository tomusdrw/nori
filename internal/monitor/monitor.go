package monitor

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"deploybot/internal/docker"
	"deploybot/internal/notify"
	"deploybot/internal/store"
)

type Monitor struct {
	store    *store.Store
	docker   docker.Client
	notify   notify.Notifier
	interval time.Duration
	cooldown time.Duration
	// per-service state tracked in-memory in a single goroutine context.
	states map[int64]serviceState
}

type serviceState struct {
	up              bool
	consecutiveDown int
	// downAlertSent records that a down alert was sent for the current
	// episode, so a recovery alert should fire when the service returns.
	downAlertSent bool
	lastDownAlert time.Time
}

func New(st *store.Store, dk docker.Client, nf notify.Notifier, interval time.Duration) *Monitor {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &Monitor{
		store:    st,
		docker:   dk,
		notify:   nf,
		interval: interval,
		cooldown: 15 * time.Minute,
		states:   make(map[int64]serviceState),
	}
}

// Run starts the monitoring loop.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	m.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Tick(ctx)
		}
	}
}

// Tick performs a single observation pass. It is callable from tests.
func (m *Monitor) Tick(ctx context.Context) {
	svcs, err := m.store.ListServices(ctx)
	if err != nil {
		log.Printf("monitor: list services: %v", err)
		return
	}
	for _, svc := range svcs {
		containers, err := m.docker.ListByService(ctx, svc.Name)
		if err != nil {
			log.Printf("monitor: list containers for %q: %v", svc.Name, err)
			continue
		}
		up := false
		for _, c := range containers {
			if c.State == "running" && c.Health != "unhealthy" {
				up = true
				break
			}
		}
		var firstDigest string
		if len(containers) > 0 {
			firstDigest = containers[0].Digest
		}
		digest := shortDigest(firstDigest)

		st := m.states[svc.ID]
		now := time.Now().UTC()

		if up {
			if !st.up && st.downAlertSent {
				m.maybeSendRecovery(ctx, svc, firstDigest)
			}
			st.up = true
			st.consecutiveDown = 0
			st.downAlertSent = false
			m.states[svc.ID] = st
			continue
		}

		// Down path: first observation after up counts as first tick
		if st.up {
			st.consecutiveDown = 1
			st.up = false
		} else {
			st.consecutiveDown++
		}
		// determine reason text
		reason := "no containers running"
		if len(containers) > 0 {
			c := containers[0]
			if c.Health == "unhealthy" {
				reason = "container " + c.Name + " unhealthy"
			} else if c.State != "running" {
				if c.ExitCode != 0 {
					reason = "container " + c.Name + " exited (code " + strconv.Itoa(c.ExitCode) + ")"
				} else {
					reason = "container " + c.Name + " state: " + c.State
				}
			}
		} else {
			reason = "no containers"
		}

		// Alert only on the second consecutive down observation, at most once
		// per cooldown window; a sustained outage does not re-alert.
		if st.consecutiveDown == 2 &&
			(st.lastDownAlert.IsZero() || now.Sub(st.lastDownAlert) >= m.cooldown) &&
			m.shouldSendNotify(ctx, store.TriggerMonitor) {
			st.lastDownAlert = now
			st.downAlertSent = true
			evt := notify.Event{
				BotName:     m.store.BotName(ctx),
				ServiceName: svc.Name,
				Trigger:     store.TriggerMonitor,
				Digest:      digest,
				Reason:      reason,
			}
			_ = m.notify.NotifyServiceDown(ctx, evt)
		}

		m.states[svc.ID] = st
	}
}

// shortDigest mirrors executor.shortDigest logic but kept local to this package.
func shortDigest(digest string) string {
	if digest == "" {
		return ""
	}
	const prefix = "sha256:"
	if strings.HasPrefix(digest, prefix) && len(digest) > len(prefix)+12 {
		return digest[:len(prefix)+12]
	}
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

// shouldSendNotify runs the gating logic for monitor notifications with a safe
// 15s timeout like the executor's alert path. Returns true if a notification
// should be sent for the given trigger.
func (m *Monitor) shouldSendNotify(ctx context.Context, trigger string) bool {
	// Build a short-lived ctx to query the notification mode, but do not block
	// the caller excessively.
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw := m.store.NotifyMode(c)
	mode, err := notify.NormalizeMode(raw)
	if err != nil {
		log.Printf("monitor: ignoring invalid stored mode %q: %v", raw, err)
		mode = notify.DefaultMode
	}
	return notify.ShouldSend(mode, trigger)
}

// maybeSendRecovery emits the recovery half of a down episode. It is only
// called for services whose down alert was actually sent.
func (m *Monitor) maybeSendRecovery(ctx context.Context, svc *store.Service, digest string) {
	if !m.shouldSendNotify(ctx, store.TriggerMonitor) {
		return
	}
	evt := notify.Event{
		BotName:     m.store.BotName(ctx),
		ServiceName: svc.Name,
		Trigger:     store.TriggerMonitor,
		Digest:      shortDigest(digest),
		Reason:      "service recovered",
	}
	_ = m.notify.NotifyServiceRecovered(ctx, evt)
}
