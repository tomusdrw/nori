package monitor

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nori/internal/docker"
	"nori/internal/notify"
	"nori/internal/store"
)

type Monitor struct {
	store      *store.Store
	docker     docker.Client
	notify     notify.Notifier
	interval   time.Duration
	cooldown   time.Duration
	httpClient *http.Client
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
		store:      st,
		docker:     dk,
		notify:     nf,
		interval:   interval,
		cooldown:   15 * time.Minute,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		states:     make(map[int64]serviceState),
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

// CheckAfterRestart gives services time to recover and returns one summary of
// any remaining problems. Call it before Run starts using the same monitor
// so an outage already counted here does not trigger an individual alert.
func (m *Monitor) CheckAfterRestart(ctx context.Context) string {
	svcs, err := m.store.ListServices(ctx)
	if err != nil {
		return "Nori restarted, but the service list could not be loaded: " + err.Error()
	}
	issues := make([]string, len(svcs))
	for i, svc := range svcs {
		issues[i] = svc.Name + " (health check did not finish)"
	}

	ticker := time.NewTicker(min(m.interval, 5*time.Second))
	defer ticker.Stop()
	for {
		for i, svc := range svcs {
			if ctx.Err() != nil {
				break
			}
			health, err := m.observe(ctx, svc, true)
			if ctx.Err() != nil {
				break
			}
			switch {
			case err != nil:
				log.Printf("monitor: list containers for %q: %v", svc.Name, err)
				issues[i] = svc.Name + " (container status unavailable)"
			case health.up:
				issues[i] = ""
			default:
				issues[i] = svc.Name
			}
		}
		var problems []string
		for _, issue := range issues {
			if issue != "" {
				problems = append(problems, issue)
			}
		}
		if len(problems) == 0 {
			return ""
		}
		select {
		case <-ctx.Done():
			for i, issue := range issues {
				if issue != "" {
					// A healthy observation resets the episode and permits
					// an individual alert for a later outage.
					m.states[svcs[i].ID] = serviceState{consecutiveDown: 2}
				}
			}
			return "Nori restarted, but these services did not pass startup checks: " + strings.Join(problems, ", ")
		case <-ticker.C:
		}
	}
}

type serviceHealth struct {
	containers   []docker.Container
	up           bool
	probeFailure string
}

func (m *Monitor) observe(ctx context.Context, svc *store.Service, starting bool) (serviceHealth, error) {
	containers, err := m.docker.ListByService(ctx, svc.Name)
	if err != nil {
		return serviceHealth{}, err
	}
	health := serviceHealth{containers: containers}
	for _, c := range containers {
		if c.State == "running" && c.Health != "unhealthy" && (!starting || c.Health != "starting") {
			health.up = true
			break
		}
	}
	if health.up && svc.HealthURL != "" {
		probeUp, detail := m.probe(ctx, svc.HealthURL)
		health.up = probeUp
		if !probeUp {
			health.probeFailure = "health probe GET " + svc.HealthURL + ": " + detail
		}
	}
	return health, nil
}

// Tick performs a single observation pass. It is callable from tests.
func (m *Monitor) Tick(ctx context.Context) {
	svcs, err := m.store.ListServices(ctx)
	if err != nil {
		log.Printf("monitor: list services: %v", err)
		return
	}
	for _, svc := range svcs {
		if ctx.Err() != nil {
			return
		}
		health, err := m.observe(ctx, svc, false)
		if err != nil {
			log.Printf("monitor: list containers for %q: %v", svc.Name, err)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		containers := health.containers
		var firstDigest string
		if len(containers) > 0 {
			firstDigest = containers[0].Digest
		}
		digest := shortDigest(firstDigest)

		st := m.states[svc.ID]
		now := time.Now().UTC()

		if health.up {
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

		if health.probeFailure != "" {
			reason = health.probeFailure
		}

		// Alert only on the second consecutive down observation, at most once
		// per cooldown window; a sustained outage does not re-alert. Whether
		// a channel actually delivers the event is decided downstream by the
		// notifier chain (notify.Route).
		if st.consecutiveDown == 2 &&
			(st.lastDownAlert.IsZero() || now.Sub(st.lastDownAlert) >= m.cooldown) {
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

// probe fetches the service health URL and reports whether it answered with
// a 2xx status, plus a short one-line detail for the failure case.
func (m *Monitor) probe(ctx context.Context, endpoint string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false, shortErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "status " + strconv.Itoa(resp.StatusCode)
	}
	return true, ""
}

// shortErr trims noisy URL-error wrappers so the SMS reason stays one line.
func shortErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	const max = 80
	if len(msg) > max {
		msg = msg[:max-1] + "…"
	}
	return msg
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

// maybeSendRecovery emits the recovery half of a down episode. It is only
// called for services the monitor alerted down for; whether a channel
// receives either half is decided downstream by the notifier chain.
func (m *Monitor) maybeSendRecovery(ctx context.Context, svc *store.Service, digest string) {
	evt := notify.Event{
		BotName:     m.store.BotName(ctx),
		ServiceName: svc.Name,
		Trigger:     store.TriggerMonitor,
		Digest:      shortDigest(digest),
		Reason:      "service recovered",
	}
	_ = m.notify.NotifyServiceRecovered(ctx, evt)
}
