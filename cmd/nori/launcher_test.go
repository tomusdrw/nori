package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"nori/internal/launcher"
)

type launcherRunner struct{}

func (launcherRunner) Run(context.Context, string, ...string) error { return nil }
func (launcherRunner) Output(context.Context, string, ...string) (string, error) {
	return "", errors.New("not found")
}

func TestRunUpPrefersManagedSelfImageOverGenericImage(t *testing.T) {
	t.Setenv("NORI_SELF_IMAGE", "ghcr.io/acme/nori:self")
	t.Setenv("NORI_IMAGE", "ghcr.io/acme/nori:generic")
	t.Setenv("NORI_ADMIN_HASH", "already-hashed")
	l := &launcher.Launcher{
		ConfigDir: t.TempDir(),
		Runner:    launcherRunner{},
		Random:    bytes.NewReader(bytes.Repeat([]byte{7}, 64)),
	}

	if err := runUp(context.Background(), l, []string{"--no-port"}); err != nil {
		t.Fatal(err)
	}
	spec, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Image != "ghcr.io/acme/nori:self" {
		t.Fatalf("image = %q, want managed self image", spec.Image)
	}
}
