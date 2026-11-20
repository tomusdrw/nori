package web

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"nori/internal/backup"
	"nori/internal/mcpauth"
)

type mcpBackupS3 struct {
	Endpoint        string `json:"endpoint,omitempty"`
	VirtualHosted   bool   `json:"virtual_hosted,omitempty"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty" jsonschema:"Write-only; omit to keep the existing secret for the same destination and access key ID."`
	Disabled        bool   `json:"disabled,omitempty"`
	ReadOnly        bool   `json:"read_only,omitempty"`
}

type mcpBackupDisk struct {
	WarningPercent  int64    `json:"warning_percent"`
	WarningBytes    int64    `json:"warning_bytes"`
	CriticalPercent int64    `json:"critical_percent"`
	CriticalBytes   int64    `json:"critical_bytes"`
	Paths           []string `json:"paths,omitempty"`
}

type mcpS3Page struct {
	Cursor string `json:"cursor,omitempty"`
}

func (s *Server) mcpBackupManager() (*backup.Manager, error) {
	if s.backups == nil || s.backups.manager == nil {
		return nil, errors.New("backups are unavailable on this instance")
	}
	return s.backups.manager, nil
}

func (s *Server) addMCPBackupAdministration(server *mcp.Server) {
	addNoriTool(s, server, "get_backup_storage", "Read S3 backup storage settings and whether a secret key is configured. Requires nori:backup_admin and nori:read.", mcpauth.ScopeRead, func(ctx context.Context, _ struct{}) (any, error) {
		if err := mcpauth.RequireScope(ctx, mcpauth.ScopeBackupAdmin); err != nil {
			return nil, err
		}
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		c, err := manager.S3(ctx)
		if err != nil {
			return nil, errors.New("could not read backup storage settings")
		}
		return map[string]any{
			"endpoint": c.Endpoint, "virtual_hosted": c.VirtualHosted, "region": c.Region,
			"bucket": c.Bucket, "prefix": c.Prefix, "access_key_id": c.AccessKeyID,
			"disabled": c.Disabled, "read_only": c.ReadOnly, "secret_configured": c.SecretAccessKey != "",
		}, nil
	})
	addNoriTool(s, server, "set_backup_storage", "Replace the S3 backup settings using the same validation as the dashboard. Requires nori:backup_admin and nori:write.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpBackupS3) (any, error) {
		if err := mcpauth.RequireScope(ctx, mcpauth.ScopeBackupAdmin); err != nil {
			return nil, err
		}
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		if err := manager.SaveS3(ctx, backup.S3Config(in)); err != nil {
			var fieldErr *backup.S3FieldError
			if errors.As(err, &fieldErr) {
				return nil, fieldErr
			}
			return nil, errors.New("could not save backup storage settings")
		}
		return map[string]bool{"saved": true}, nil
	})
	addNoriTool(s, server, "get_backup_recovery_key", "Retrieve the private recovery key, if managed by Nori. Store it securely. Requires nori:backup_admin and nori:read.", mcpauth.ScopeRead, func(ctx context.Context, _ struct{}) (any, error) {
		if err := mcpauth.RequireScope(ctx, mcpauth.ScopeBackupAdmin); err != nil {
			return nil, err
		}
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		key := manager.RecoveryKey()
		if key == "" {
			return nil, errors.New("no instance recovery key is available")
		}
		return map[string]string{"recovery_key": key}, nil
	})
	addNoriTool(s, server, "list_remote_backups", "List a page of backup archive keys under the configured S3 prefix. Pass next_cursor to list another page.", mcpauth.ScopeRead, func(ctx context.Context, in mcpS3Page) (any, error) {
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		objects, next, err := manager.S3Objects(ctx, in.Cursor)
		if err != nil {
			return nil, errors.New("could not list remote backups")
		}
		return map[string]any{"keys": objects, "next_cursor": next}, nil
	})
	addNoriTool(s, server, "get_backup_disks", "Read backup disk usage and free-space alert thresholds.", mcpauth.ScopeRead, func(ctx context.Context, _ struct{}) (any, error) {
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		policy, err := manager.DiskPolicy(ctx)
		if err != nil {
			return nil, errors.New("could not read backup disk policy")
		}
		disks, err := manager.Disks(ctx)
		if err != nil {
			return nil, errors.New("could not measure backup disks")
		}
		return map[string]any{"policy": mcpBackupDisk(policy), "disks": disks}, nil
	})
	addNoriTool(s, server, "set_backup_disk_policy", "Replace backup disk warning and critical thresholds and optional additional monitor paths.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpBackupDisk) (any, error) {
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		if err := manager.SaveDiskPolicy(ctx, backup.DiskPolicy(in)); err != nil {
			return nil, err
		}
		return map[string]bool{"saved": true}, nil
	})
}
