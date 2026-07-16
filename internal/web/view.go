package web

import (
	"strings"
	"time"

	"deploybot/internal/docker"
)

type ServiceView struct {
	ID              int64
	Name            string
	State           string
	RunningVersion  string
	LatestVersion   string
	RunningFor      string
	LastDeploy      string
	RecentLogs      string
	UpdateAvailable bool
	Managed         bool
}

func repoOf(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		ref = ref[:colon]
	}
	return ref
}

func watchedDigest(cs []docker.Container, watchedImage string) string {
	want := repoOf(watchedImage)
	for _, c := range cs {
		if repoOf(c.Image) == want {
			return c.Digest
		}
	}
	return ""
}

func summarizeState(cs []docker.Container) string {
	if len(cs) == 0 {
		return "none"
	}
	running := 0
	for _, c := range cs {
		if c.State == "running" {
			running++
		}
	}
	switch {
	case running == 0:
		return "stopped"
	case running == len(cs):
		return "running"
	default:
		return "partial"
	}
}

func shortDigest(d string) string {
	if d == "" {
		return "—"
	}
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

func runningFor(cs []docker.Container, now time.Time) string {
	var earliest time.Time
	hasRunning := false
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		hasRunning = true
		if c.StartedAt.IsZero() {
			continue
		}
		if earliest.IsZero() || c.StartedAt.Before(earliest) {
			earliest = c.StartedAt
		}
	}
	if earliest.IsZero() {
		if hasRunning {
			return "Duration unavailable"
		}
		return "Not running"
	}
	return timeSince(now.Sub(earliest))
}

func timeSince(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconvItoa(int(d.Round(time.Minute)/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconvItoa(int(d.Round(time.Hour)/time.Hour)) + "h"
	default:
		return strconvItoa(int(d.Round(24*time.Hour)/(24*time.Hour))) + "d"
	}
}

func deployedAgo(startedAt time.Time, now time.Time) string {
	if startedAt.IsZero() {
		return "No deployments yet"
	}
	return timeSince(now.Sub(startedAt)) + " ago"
}
