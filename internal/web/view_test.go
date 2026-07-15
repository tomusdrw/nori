package web

import (
	"testing"

	"deploybot/internal/docker"
)

func TestRepoOf(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/me/app:latest":     "ghcr.io/me/app",
		"ghcr.io/me/app":            "ghcr.io/me/app",
		"ghcr.io/me/app@sha256:abc": "ghcr.io/me/app",
		"localhost:5000/me/app:v1":  "localhost:5000/me/app",
	}
	for in, want := range cases {
		if got := repoOf(in); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarizeState(t *testing.T) {
	if summarizeState(nil) != "none" {
		t.Error("nil should be none")
	}
	all := []docker.Container{{State: "running"}, {State: "running"}}
	if summarizeState(all) != "running" {
		t.Error("all running")
	}
	none := []docker.Container{{State: "exited"}, {State: "exited"}}
	if summarizeState(none) != "stopped" {
		t.Error("all stopped")
	}
	mix := []docker.Container{{State: "running"}, {State: "exited"}}
	if summarizeState(mix) != "partial" {
		t.Error("mixed = partial")
	}
}

func TestWatchedDigest(t *testing.T) {
	cs := []docker.Container{
		{Image: "postgres:16", Digest: "sha256:db"},
		{Image: "ghcr.io/me/app:latest", Digest: "sha256:app"},
	}
	if got := watchedDigest(cs, "ghcr.io/me/app:latest"); got != "sha256:app" {
		t.Errorf("watchedDigest = %q, want sha256:app", got)
	}
	if got := watchedDigest(cs, "ghcr.io/me/other:latest"); got != "" {
		t.Errorf("watchedDigest = %q, want empty", got)
	}
}

func TestShortDigest(t *testing.T) {
	if got := shortDigest("sha256:9f3c1a2bdeadbeef"); got != "9f3c1a2bdead" {
		t.Errorf("shortDigest = %q", got)
	}
	if got := shortDigest(""); got != "—" {
		t.Errorf("empty shortDigest = %q, want dash", got)
	}
}
