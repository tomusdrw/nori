package terminal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAvailableRequiresTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	manager := New("test", t.TempDir())
	if err := manager.Available(); err == nil || !strings.Contains(err.Error(), "tmux") {
		t.Fatalf("Available error = %v", err)
	}
}

func TestAvailableRequiresWorkingDirectory(t *testing.T) {
	binDir := t.TempDir()
	tmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	manager := New("test", filepath.Join(t.TempDir(), "missing"))
	if err := manager.Available(); err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("Available error = %v", err)
	}
}

func TestAvailableRequiresBash(t *testing.T) {
	binDir := t.TempDir()
	tmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	manager := New("test", t.TempDir())
	if err := manager.Available(); err == nil || !strings.Contains(err.Error(), "bash") {
		t.Fatalf("Available error = %v", err)
	}
}

func TestNewResolvesWorkingDirectory(t *testing.T) {
	manager := New("test", ".")
	if !filepath.IsAbs(manager.workingDir) {
		t.Fatalf("workingDir = %q, want absolute path", manager.workingDir)
	}
}
