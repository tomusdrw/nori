package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const SettingMCP = "mcp"

// Epoch binds all grants to the current configuration. Changing the public
// origin, disabling MCP or revoking access invalidates previously issued tokens.
type MCPConfig struct {
	Enabled   bool   `json:"enabled"`
	PublicURL string `json:"public_url"`
	Epoch     string `json:"epoch"`
}

func NormalizeMCPPublicURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid MCP public URL")
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.Opaque != "" {
		return "", errors.New("MCP public URL must be an origin without credentials, a path, query or fragment")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid MCP public URL port")
		}
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("MCP requires HTTPS; HTTP is allowed only on loopback for local development")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = ""
	return u.String(), nil
}

func (s *Store) GetMCPConfig(ctx context.Context) (MCPConfig, error) {
	raw, err := s.GetSetting(ctx, SettingMCP)
	if errors.Is(err, ErrNotFound) {
		return MCPConfig{}, nil
	}
	if err != nil {
		return MCPConfig{}, err
	}
	return decodeMCPConfig(raw)
}

func decodeMCPConfig(raw string) (MCPConfig, error) {
	var cfg MCPConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return MCPConfig{}, err
	}
	if cfg.Enabled {
		canonical, err := NormalizeMCPPublicURL(cfg.PublicURL)
		if err != nil || canonical != cfg.PublicURL || cfg.Epoch == "" {
			return MCPConfig{}, errors.New("invalid stored MCP configuration")
		}
	}
	return cfg, nil
}

func (s *Store) SetMCPConfig(ctx context.Context, enabled bool, publicURL string, revoke bool) error {
	publicURL = strings.TrimSpace(publicURL)
	if enabled || publicURL != "" {
		var err error
		publicURL, err = NormalizeMCPPublicURL(publicURL)
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire the SQLite writer lock before reading the epoch. A concurrent
	// ordinary settings save must never restore an epoch that was just revoked.
	if _, err := tx.ExecContext(ctx, `INSERT INTO setting(key,value) VALUES (?, '{}') ON CONFLICT(key) DO NOTHING`, SettingMCP); err != nil {
		return err
	}
	var previous string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM setting WHERE key=?`, SettingMCP).Scan(&previous); err != nil {
		return err
	}
	cfg, err := decodeMCPConfig(previous)
	if err != nil {
		return err
	}
	if cfg.Enabled != enabled || cfg.PublicURL != publicURL || cfg.Epoch == "" || revoke {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		cfg.Epoch = hex.EncodeToString(nonce[:])
		// Old registrations and credentials cannot be used in the new epoch.
		// Remove them so the administrator can also recover from a full registry.
		if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_oauth`); err != nil {
			return err
		}
	}
	cfg.Enabled, cfg.PublicURL = enabled, publicURL
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE setting SET value=? WHERE key=?`, string(raw), SettingMCP); err != nil {
		return err
	}
	return tx.Commit()
}
