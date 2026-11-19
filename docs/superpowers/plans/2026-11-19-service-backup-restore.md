# Service Backup and Restore Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Make a service recoverable on the same or a different Nori instance from its local or B2 backup.

**Architecture:** Persist policy and job state in SQLite. A backup worker coordinates capture with deployment, writes a portable age-encrypted archive on the data filesystem, and uploads completed archives independently. Restore verifies the archive before it provisions fresh resources.

**Tech Stack:** Go, SQLite, age, Docker, PostgreSQL client tools, S3, templ.

**Spec:** [Approved backup design](../specs/2026-11-19-service-backup-restore-design.md)

## Global constraints

- Default to daily local backups with seven retained successes per ordinary service.
- Keep workflow in the administrator dashboard. Restore runs as a new service with manual deployment.
- Warn below 15% or 5 GiB, and defer backup writes below 5% or 1 GiB. Never prune the previous good copy to admit a write.
- Coordinate data capture and restore with deployment per service.

## Review focus

- Configuration and dotenv changed concurrently with backup.
- Archive damaged after its last expected member, or containing unsafe paths.
- Process interrupted while writers are quiesced.
- Disk fills during capture or extraction.
- B2 upload fails or is retried after restart.

### 1. Archive and local worker

- [x] Test an encrypted round trip across identities, wrong key, truncation, duplicate entries and unsafe member paths in `internal/backup/archive_test.go`.
- [x] Implement archive manifest and streaming writer/reader in `internal/backup/archive.go`; authenticate the whole stream before applying data.
- [x] Test atomic config snapshots, seven-copy retention and interrupted jobs through the store and worker in `internal/backup/service_test.go`.
- [x] Implement policy and job persistence, scheduler, archive staging and cleanup in `internal/store/backup.go` and `internal/backup/service.go`. Add bounded work and restart reconciliation.
- [x] Test low-space transition, deduplication, recovery and early/during-write guard in `internal/backup/service_test.go` and `internal/backup/restore_test.go`. Wire monitoring to configured notification channels in `cmd/nori/main.go`.
- [x] Add the local history, creation, download, key export, policy and staged configuration restore routes in `internal/web/backup.go`. Test dashboard authorization and restore to a different encryption key.
- [x] Run `go test ./internal/backup ./internal/store ./internal/web ./cmd/nori`.

### 2. Service data

- [x] Test PostgreSQL data and selected filesystem sources round trip. Add archive member metadata for source type, versions, original paths and checksums.
- [x] Implement a managed PostgreSQL dump/restore using client tools compatible with the managed database, plus explicit volume/bind capture and restore to fresh targets.
- [x] Test failed capture, restart after quiescing and failed resume. Persist a resume action before any stop and coordinate with the executor. Add custom export/import and quiesce/resume commands with bounded execution and administrator review.
- [x] Add source discovery, capture modes, restore mapping and preflight to the dashboard.
- [x] Run `go test ./...` and exercise a migration with Docker when available.

### 3. Backblaze

- [x] Test S3 upload, failed upload with local copy still downloadable, retry and import with read-only credentials.
- [x] Add optional instance B2 settings, encrypted credentials and per-service opt-out. Upload completed archives, retain durable status and allow retry. Import a selected object through the same verified archive path.
- [x] Run `make test`, `go vet ./...` and `git diff --check`. Verify mounted disk metrics and document how B2 lifecycle and recovery keys work.
