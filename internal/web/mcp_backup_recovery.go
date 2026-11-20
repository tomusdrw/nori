package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"nori/internal/backup"
	"nori/internal/envfile"
	"nori/internal/mcpauth"
)

const mcpArchivePath = "/mcp/backup-archives"

type mcpRestoreSource struct {
	BackupID    int64  `json:"backup_id,omitempty" jsonschema:"Local backup ID. Supply either backup_id or s3_key."`
	S3Key       string `json:"s3_key,omitempty" jsonschema:"Remote key from list_remote_backups."`
	RecoveryKey string `json:"recovery_key,omitempty" jsonschema:"Optional external recovery key. Never returned."`
}

type mcpArchiveDownload struct {
	BackupID int64 `json:"backup_id" jsonschema:"Local backup ID from list_service_backups."`
}

type mcpRestoreDraft struct {
	DraftID string `json:"draft_id"`
}

type mcpRestoreOptions struct {
	DraftID        string            `json:"draft_id"`
	Name           string            `json:"name" jsonschema:"New service name"`
	Network        string            `json:"network,omitempty"`
	Domain         string            `json:"domain,omitempty"`
	Targets        map[string]string `json:"targets,omitempty" jsonschema:"Destination overrides by zero-based source index from the review."`
	ReviewCommands bool              `json:"review_commands" jsonschema:"True confirms review of the imported commands and destination."`
	DeployScript   *string           `json:"deploy_script,omitempty" jsonschema:"Optional replacement deployment script for Custom services."`
}

func mcpRestoreOwner(ctx context.Context) string {
	identity, _ := mcpauth.FromContext(ctx)
	if identity.GrantFamily != "" {
		return "grant:" + identity.GrantFamily
	}
	return "client:" + identity.ClientID
}

func (s *Server) mcpRestoreDraft(ctx context.Context, token string) (*restoreDraft, error) {
	if _, err := s.mcpBackupManager(); err != nil {
		return nil, err
	}
	pages := s.backups
	pages.mu.Lock()
	defer pages.mu.Unlock()
	draft := pages.draft
	if draft == nil || draft.token != token || draft.owner != mcpRestoreOwner(ctx) || !time.Now().Before(draft.expires) {
		return nil, errors.New("restore draft not found or expired")
	}
	return draft, nil
}

func (s *Server) addMCPBackupRecovery(server *mcp.Server) {
	addNoriTool(s, server, "get_backup_archive_download", "Get the OAuth-protected HTTP download path for a completed local encrypted backup. Supports Range requests.", mcpauth.ScopeRead, func(ctx context.Context, in mcpArchiveDownload) (any, error) {
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		job, err := s.store.BackupJob(ctx, in.BackupID)
		if err != nil {
			return nil, errors.New("backup not found")
		}
		file, err := manager.Open(job)
		if err != nil {
			return nil, errors.New("local backup archive is unavailable")
		}
		file.Close()
		return map[string]any{"method": "GET", "path": fmt.Sprintf("%s?backup_id=%d", mcpArchivePath, job.ID), "size_bytes": job.Size}, nil
	})
	addNoriTool(s, server, "get_backup_archive_upload", "Get the OAuth-protected HTTP upload path for an encrypted local archive. POST the file as application/octet-stream, optionally supplying an external recovery key in the Nori-Recovery-Key header.", mcpauth.ScopeWrite, func(ctx context.Context, _ struct{}) (any, error) {
		if _, err := s.mcpBackupManager(); err != nil {
			return nil, err
		}
		return map[string]string{"method": "POST", "path": mcpArchivePath, "content_type": "application/octet-stream"}, nil
	})
	addNoriTool(s, server, "prepare_backup_restore", "Stage a local or remote backup for review. Use get_backup_restore_review before confirming the restore.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpRestoreSource) (any, error) {
		manager, err := s.mcpBackupManager()
		if err != nil {
			return nil, err
		}
		if in.BackupID < 0 || (in.BackupID == 0) == (in.S3Key == "") || len(in.RecoveryKey) > 512 {
			return nil, errors.New("supply one backup_id or s3_key and a valid optional recovery key")
		}
		ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
		defer cancel()
		var archive io.ReadCloser
		if in.BackupID > 0 {
			job, err := s.store.BackupJob(ctx, in.BackupID)
			if err != nil {
				return nil, errors.New("local backup archive not found")
			}
			archive, err = manager.Open(job)
			if err != nil {
				return nil, errors.New("local backup archive is unavailable")
			}
		} else {
			archive, err = manager.OpenS3(ctx, in.S3Key)
			if err != nil {
				return nil, err
			}
		}
		defer archive.Close()
		draft, err := s.backups.stageDraft(ctx, archive, in.RecoveryKey, mcpRestoreOwner(ctx))
		if err != nil {
			return nil, errors.New("could not stage backup; check its recovery key, integrity, available disk space, and any pending restore")
		}
		return map[string]any{"draft_id": draft.token, "expires_at": draft.expires.UTC()}, nil
	})
	addNoriTool(s, server, "get_backup_restore_review", "Review a staged backup's service configuration, commands and source indexes before restoring it. Requires the same write grant that staged the archive.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpRestoreDraft) (any, error) {
		draft, err := s.mcpRestoreDraft(ctx, in.DraftID)
		if err != nil {
			return nil, err
		}
		manifest := draft.manifest
		archived := &envfile.Redactor{}
		if err := archived.Add(manifest.Env); err != nil {
			return nil, errors.New("could not redact archived environment values")
		}
		configuration := map[string]any{
			"name": manifest.Service.Name, "description": manifest.Service.Description,
			"deployment_mode": manifest.Service.DeploymentMode, "deploy_script": manifest.Service.DeployScript,
			"template_config": manifest.Service.TemplateConfig, "watched_image": manifest.Service.WatchedImage,
		}
		return redactMCPResult(map[string]any{
			"service": configuration, "policy": mcpBackupPolicy(manifest.Policy),
			"expires_at": draft.expires.UTC(),
		}, archived, false), nil
	})
	addNoriTool(s, server, "confirm_backup_restore", "Restore the reviewed draft as a new service. Requires review_commands=true and the same write grant that staged the archive.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpRestoreOptions) (any, error) {
		if !in.ReviewCommands {
			return nil, errors.New("review the staged backup and confirm with review_commands=true")
		}
		draft, err := s.mcpRestoreDraft(ctx, in.DraftID)
		if err != nil {
			return nil, err
		}
		targets := make(map[int]string, len(in.Targets))
		for key, path := range in.Targets {
			index, err := strconv.Atoi(key)
			if err != nil || strconv.Itoa(index) != key || index < 0 || index >= len(draft.manifest.Policy.Sources) {
				return nil, errors.New("target index does not identify a source in the review")
			}
			targets[index] = strings.TrimSpace(path)
		}
		draft = s.backups.takeDraft(in.DraftID, mcpRestoreOwner(ctx))
		if draft == nil {
			return nil, errors.New("restore draft not found or expired")
		}
		ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
		defer cancel()
		svc, err := s.backups.manager.Restore(ctx, draft.manifest, draft.directory, backup.RestoreOptions{
			Name: strings.TrimSpace(in.Name), Network: strings.TrimSpace(in.Network),
			Domain: strings.TrimSpace(in.Domain), Targets: targets, ReviewCommands: true, DeployScript: in.DeployScript,
		})
		if err != nil {
			if svc.ID != 0 || !s.backups.returnDraft(draft) {
				draft.cleanup()
			}
			return nil, err
		}
		draft.cleanup()
		return map[string]any{"service_id": svc.ID, "name": svc.Name}, nil
	})
	addNoriTool(s, server, "cancel_backup_restore", "Discard the staged restore and release its temporary files. Requires the same write grant that staged the archive.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpRestoreDraft) (any, error) {
		if _, err := s.mcpBackupManager(); err != nil {
			return nil, err
		}
		draft := s.backups.takeDraft(in.DraftID, mcpRestoreOwner(ctx))
		if draft == nil {
			return nil, errors.New("restore draft not found or expired")
		}
		draft.cleanup()
		return map[string]bool{"cancelled": true}, nil
	})
}

func (s *Server) handleMCPBackupArchive(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if err := mcpauth.RequireScope(r.Context(), mcpauth.ScopeRead); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if _, err := s.mcpBackupManager(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		id, err := strconv.ParseInt(r.URL.Query().Get("backup_id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid backup_id", http.StatusBadRequest)
			return
		}
		job, err := s.store.BackupJob(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.serveBackupArchive(w, r, job)
	case http.MethodPost:
		s.handleMCPBackupUpload(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPBackupUpload(w http.ResponseWriter, r *http.Request) {
	if err := mcpauth.RequireScope(r.Context(), mcpauth.ScopeWrite); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if _, err := s.mcpBackupManager(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	key := r.Header.Get("Nori-Recovery-Key")
	if err != nil || contentType != "application/octet-stream" || len(key) > 512 || len(r.Header.Values("Nori-Recovery-Key")) > 1 {
		http.Error(w, "invalid archive content type or recovery key header", http.StatusBadRequest)
		return
	}
	controller := http.NewResponseController(w)
	defer controller.SetReadDeadline(time.Time{})
	r.Body = &backupUploadBody{ReadCloser: r.Body, controller: controller}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<40)
	draft, err := s.backups.stageDraft(r.Context(), r.Body, key, mcpRestoreOwner(r.Context()))
	if err != nil {
		http.Error(w, "could not stage backup; check its recovery key, integrity, available disk space, and any pending restore", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"draft_id": draft.token, "expires_at": draft.expires.UTC()})
}
