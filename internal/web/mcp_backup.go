package web

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"nori/internal/mcpauth"
	"nori/internal/store"
)

const mcpBackupInstructions = "When creating or updating a service with a database or persistent storage, check its backup policy using get_service_backup_policy. Recommend a schedule, retention, and sources appropriate to the service, and use update_service_backup_policy to apply the user's backup preferences. Review existing settings before making changes. Include only data belonging to the service. Managed PostgreSQL is backed up logically by default. For other live databases, check how to capture consistent data before selecting filesystem sources. A policy permits S3 uploads only when s3_upload_enabled is true and local_only is false. Check list_service_backups for upload status before reporting that a copy has reached S3."

type mcpBackupSource struct {
	Type string `json:"type" jsonschema:"volume, managed, or bind"`
	Path string `json:"path" jsonschema:"Docker volume name, logical template volume name, or absolute bind directory"`
}

type mcpBackupUpdate struct {
	ServiceID    int64              `json:"service_id" jsonschema:"Service ID"`
	Schedule     *string            `json:"schedule,omitempty" jsonschema:"Backup cron expression in the server timezone. Empty string selects manual backups."`
	Keep         *int               `json:"keep,omitempty" jsonschema:"Number of local copies to retain, from 1 to 100. Reducing this can expire older copies immediately."`
	LocalOnly    *bool              `json:"local_only,omitempty" jsonschema:"Keep backups local. False allows uploads if the instance has writable S3 storage configured."`
	SkipPostgres *bool              `json:"skip_postgres,omitempty" jsonschema:"Exclude the managed PostgreSQL database. The default is to include a logical database dump."`
	Sources      *[]mcpBackupSource `json:"sources,omitempty" jsonschema:"Replace the filesystem source list. An empty list clears it. Select at most 32 sources."`
	StopApp      *bool              `json:"stop_app,omitempty" jsonschema:"Stop the managed application container during data capture. This causes temporary downtime."`
	Quiesce      *string            `json:"quiesce,omitempty" jsonschema:"Shell command to pause writes before capture. Requires resume. Set both to empty strings to clear; omit to preserve. Use environment variables for secrets."`
	Resume       *string            `json:"resume,omitempty" jsonschema:"Retry-safe shell command to resume writes after capture. Requires quiesce. Use environment variables for secrets."`
	Export       *string            `json:"export,omitempty" jsonschema:"Shell command writing backup data to $NORI_BACKUP_WORK. Requires import. Set both to empty strings to clear; omit to preserve. Use environment variables for secrets."`
	Import       *string            `json:"import,omitempty" jsonschema:"Shell command restoring data from $NORI_BACKUP_WORK. Requires export. Use environment variables for secrets."`
}

type mcpBackupJob struct {
	ServiceID int64 `json:"service_id"`
	BackupID  int64 `json:"backup_id" jsonschema:"Backup ID returned by list_service_backups"`
}

type mcpBackupHistory struct {
	ServiceID int64 `json:"service_id,omitempty" jsonschema:"Omit to list recent backups for the whole instance, including deleted services."`
}

func (in mcpBackupUpdate) apply(policy *store.BackupPolicy) {
	if in.Schedule != nil {
		policy.Schedule = strings.TrimSpace(*in.Schedule)
	}
	if in.Keep != nil {
		policy.Keep = *in.Keep
	}
	if in.LocalOnly != nil {
		policy.LocalOnly = *in.LocalOnly
	}
	if in.SkipPostgres != nil {
		policy.SkipPostgres = *in.SkipPostgres
	}
	if in.Sources != nil {
		policy.Sources = make([]store.BackupSource, 0, len(*in.Sources))
		for _, source := range *in.Sources {
			policy.Sources = append(policy.Sources, store.BackupSource{Type: source.Type, Path: source.Path})
		}
	}
	if in.StopApp != nil {
		policy.StopApp = *in.StopApp
	}
	if in.Quiesce != nil {
		policy.Quiesce = *in.Quiesce
	}
	if in.Resume != nil {
		policy.Resume = *in.Resume
	}
	if in.Export != nil {
		policy.Export = *in.Export
	}
	if in.Import != nil {
		policy.Import = *in.Import
	}
}

func mcpBackupSources(sources []store.BackupSource) []mcpBackupSource {
	result := make([]mcpBackupSource, 0, len(sources))
	for _, source := range sources {
		result = append(result, mcpBackupSource{Type: source.Type, Path: source.Path})
	}
	return result
}

func mcpBackupPolicy(policy store.BackupPolicy) map[string]any {
	return map[string]any{
		"schedule":          policy.Schedule,
		"keep":              policy.Keep,
		"local_only":        policy.LocalOnly,
		"skip_postgres":     policy.SkipPostgres,
		"sources":           mcpBackupSources(policy.Sources),
		"stop_app":          policy.StopApp,
		"quiesce":           policy.Quiesce,
		"resume":            policy.Resume,
		"export":            policy.Export,
		"import":            policy.Import,
		"has_quiesce_hooks": policy.Quiesce != "" || policy.Resume != "",
		"has_data_hooks":    policy.Export != "" || policy.Import != "",
	}
}

func (s *Server) mcpBackupService(ctx context.Context, id int64) (*store.Service, error) {
	if s.backups == nil || s.backups.manager == nil {
		return nil, errors.New("backups are unavailable on this instance")
	}
	svc, err := s.mcpService(ctx, id, false)
	if err != nil {
		return nil, err
	}
	if svc.IsSelf {
		return nil, errors.New("backup policies are available only for ordinary services")
	}
	return svc, nil
}

func (s *Server) addMCPBackupTools(server *mcp.Server) {
	addNoriTool(s, server, "get_service_backup_policy",
		"Read a service's backup policy including custom commands, eligible detected mounts, and whether instance S3 uploads are enabled. Known environment values are redacted. Also returns the effective defaults for a service without a saved policy.",
		mcpauth.ScopeRead, func(ctx context.Context, in mcpID) (any, error) {
			svc, err := s.mcpBackupService(ctx, in.ServiceID)
			if err != nil {
				return nil, err
			}
			policy, _, err := s.store.BackupPolicy(ctx, svc.ID)
			if err != nil {
				return nil, errors.New("could not read backup policy")
			}
			manager := s.backups.manager
			s3, err := manager.S3(ctx)
			if err != nil {
				return nil, errors.New("could not read S3 upload status")
			}
			detectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			sources, err := manager.Discover(detectCtx, *svc)
			return map[string]any{
				"policy":                  mcpBackupPolicy(policy),
				"s3_upload_enabled":       s3.UploadEnabled(),
				"detected_sources":        mcpBackupSources(sources),
				"source_discovery_failed": err != nil,
			}, nil
		})

	addNoriTool(s, server, "update_service_backup_policy",
		"Update the supplied backup settings and custom commands for a service and return the saved policy. Omitted fields are preserved. Read get_service_backup_policy before updating. Use service environment variables for credentials.",
		mcpauth.ScopeWrite, func(ctx context.Context, in mcpBackupUpdate) (any, error) {
			svc, err := s.mcpBackupService(ctx, in.ServiceID)
			if err != nil {
				return nil, err
			}
			policy, err := s.backups.manager.UpdatePolicy(ctx, *svc, in.apply)
			if err != nil {
				return nil, err
			}
			return mcpBackupPolicy(policy), nil
		})

	addNoriTool(s, server, "create_service_backup",
		"Queue an immediate backup using the service's current policy. Returns the backup ID. Check progress with list_service_backups.",
		mcpauth.ScopeWrite, func(ctx context.Context, in mcpID) (any, error) {
			svc, err := s.mcpBackupService(ctx, in.ServiceID)
			if err != nil {
				return nil, err
			}
			id, err := s.backups.manager.QueueID(ctx, *svc)
			if err != nil {
				return nil, errors.New("could not queue backup")
			}
			if id == 0 {
				return nil, errors.New("could not queue backup; check for an active backup or incomplete restore")
			}
			return map[string]int64{"backup_id": id}, nil
		})

	addNoriTool(s, server, "list_service_backups",
		"List up to 100 recent backups, including capture and upload status and failure messages. Omit service_id for instance-wide history.",
		mcpauth.ScopeRead, func(ctx context.Context, in mcpBackupHistory) (any, error) {
			if _, err := s.mcpBackupManager(); err != nil {
				return nil, err
			}
			if in.ServiceID < 0 {
				return nil, errors.New("invalid service_id")
			}
			var jobs []store.BackupJob
			var err error
			if in.ServiceID == 0 {
				jobs, err = s.store.RecentBackups(ctx)
			} else {
				jobs, err = s.store.BackupHistory(ctx, in.ServiceID)
			}
			if err != nil {
				return nil, errors.New("could not read backup history")
			}
			history := make([]map[string]any, 0, len(jobs))
			for _, job := range jobs {
				history = append(history, map[string]any{
					"backup_id": job.ID, "service_id": job.ServiceID, "service_name": job.ServiceName, "created_at": job.Created.UTC(),
					"status": job.Status, "size_bytes": job.Size, "message": job.Message,
					"upload_status": job.UploadStatus, "upload_attempts": job.UploadAttempts,
				})
			}
			return map[string]any{"backups": history}, nil
		})

	addNoriTool(s, server, "retry_service_backup_upload",
		"Queue another S3 upload for a completed local backup. Requires writable S3 storage and a policy that permits uploads.",
		mcpauth.ScopeWrite, func(ctx context.Context, in mcpBackupJob) (any, error) {
			manager, err := s.mcpBackupManager()
			if err != nil {
				return nil, err
			}
			job, err := s.store.BackupJob(ctx, in.BackupID)
			if err != nil || job.ServiceID != in.ServiceID {
				return nil, errors.New("backup not found for this service")
			}
			if err := manager.RetryUpload(ctx, job); err != nil {
				return nil, err
			}
			return map[string]int64{"backup_id": job.ID}, nil
		})
}
