package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMCPConfigLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.db")
	s, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	cfg, err := s.GetMCPConfig(ctx)
	if err != nil || cfg.Enabled {
		t.Fatalf("default: %+v %v", cfg, err)
	}
	if err := s.SetMCPConfig(ctx, true, "https://nori.example/", false); err != nil {
		t.Fatal(err)
	}
	first, _ := s.GetMCPConfig(ctx)
	if !first.Enabled || first.PublicURL != "https://nori.example" || first.Epoch == "" {
		t.Fatalf("enabled: %+v", first)
	}
	if err := s.SetMCPConfig(ctx, true, first.PublicURL, false); err != nil {
		t.Fatal(err)
	}
	same, _ := s.GetMCPConfig(ctx)
	if same.Epoch != first.Epoch {
		t.Fatal("saving unrelated settings revoked access")
	}
	if err := s.SetMCPConfig(ctx, false, first.PublicURL, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMCPConfig(ctx, true, first.PublicURL, false); err != nil {
		t.Fatal(err)
	}
	back, _ := s.GetMCPConfig(ctx)
	if back.Epoch == first.Epoch {
		t.Fatal("re-enabling restored old grants")
	}
	if err := s.SetMCPConfig(ctx, true, first.PublicURL, true); err != nil {
		t.Fatal(err)
	}
	revoked, _ := s.GetMCPConfig(ctx)
	if revoked.Epoch == back.Epoch {
		t.Fatal("revoke did not change epoch")
	}
	s.Close()
	s, err = Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reopened, _ := s.GetMCPConfig(ctx)
	if reopened != revoked {
		t.Fatalf("settings lost on restart: %+v", reopened)
	}
}

func TestMCPPublicURLValidation(t *testing.T) {
	for _, raw := range []string{"", "http://nori.example", "https://user:pass@nori.example", "https://nori.example/path", "https://nori.example?x=1", "https://nori.example#fragment", "//nori.example", "https://nori.example:invalid", "https://nori.example\\evil"} {
		if _, err := NormalizeMCPPublicURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"https://nori.example", "https://nori.example:8443/", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := NormalizeMCPPublicURL(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
}
