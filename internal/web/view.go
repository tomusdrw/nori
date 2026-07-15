package web

import (
	"strings"

	"deploybot/internal/docker"
)

type ServiceView struct {
	ID              int64
	Name            string
	State           string
	RunningVersion  string
	LatestVersion   string
	UpdateAvailable bool
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
