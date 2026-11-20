package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"nori/internal/backup"
	"nori/internal/store"
)

type backupPages struct {
	manager *backup.Manager
	mu      sync.Mutex
	draft   *restoreDraft
}

type restoreDraft struct {
	token     string
	owner     string
	manifest  backup.Manifest
	directory string
	expires   time.Time
	cleanup   func()
}

type backupPageData struct {
	Service      *store.Service
	Policy       store.BackupPolicy
	Detected     []store.BackupSource
	Jobs         []store.BackupJob
	Disks        []backup.DiskUsage
	DiskPolicy   backup.DiskPolicy
	S3           backup.S3Config
	S3Saved      bool
	KeyAvailable bool
}

func (s *Server) SetBackups(m *backup.Manager) {
	s.backups = &backupPages{manager: m}
}

func (s *Server) backupRoutes(r chi.Router) {
	r.Get("/backups", s.handleBackups)
	r.Get("/backups/{id}/download", s.handleBackupDownload)
	r.Get("/backups/s3", s.handleS3Objects)
	r.Get("/backups/restore/{token}", s.handleRestoreReview)
	r.Get("/services/{name}/backups", s.handleBackups)
	r.Post("/services/{name}/backups", s.handleBackupPolicy)
	r.Post("/services/{name}/backups/create", s.handleBackupCreate)
	r.Post("/backups/{id}/retry", s.handleBackupRetry)
	r.Post("/backups/{id}/import", s.handleLocalImport)
	r.Post("/backups/s3/import", s.handleS3Import)
	r.Post("/backups/s3", s.handleS3Config)
	r.Post("/backups/disks", s.handleBackupDiskPolicy)
	r.Post("/backups/key", s.handleBackupKey)
	r.Post("/backups/restore/{token}", s.handleRestoreConfirm)
}

func (s *Server) backupService(w http.ResponseWriter, r *http.Request) (*store.Service, bool) {
	svc, err := s.store.GetServiceByName(r.Context(), chi.URLParam(r, "name"))
	if err != nil || svc.IsSelf {
		http.NotFound(w, r)
		return nil, false
	}
	return svc, true
}

func serviceBackupsURL(name string) string {
	return "/services/" + url.PathEscape(name) + "/backups"
}

func backupJobURL(id int64) string { return "/backups/" + strconv.FormatInt(id, 10) }

func (s *Server) backupJob(w http.ResponseWriter, r *http.Request) (store.BackupJob, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err == nil {
		job, err := s.store.BackupJob(r.Context(), id)
		if err == nil {
			return job, true
		}
	}
	http.NotFound(w, r)
	return store.BackupJob{}, false
}

func (s *Server) requireBackups(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if s.backups == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	ctx := r.Context()
	m := s.backups.manager
	var data backupPageData
	var err error
	if chi.URLParam(r, "name") != "" {
		svc, ok := s.backupService(w, r)
		if !ok {
			return
		}
		data.Service = svc
		data.Policy, _, err = s.store.BackupPolicy(ctx, svc.ID)
		if err == nil {
			data.Jobs, err = s.store.BackupHistory(ctx, svc.ID)
		}
		detectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		data.Detected, _ = m.Discover(detectCtx, *svc)
	} else {
		data.Jobs, err = s.store.RecentBackups(ctx)
		if err == nil {
			data.Disks, err = m.Disks(ctx)
		}
		if err == nil {
			data.DiskPolicy, err = m.DiskPolicy(ctx)
		}
		data.KeyAvailable = m.RecoveryKey() != ""
		data.S3Saved = r.URL.Query().Get("s3_saved") == "1"
	}
	if err == nil {
		data.S3, err = m.S3(ctx)
		data.S3.SecretAccessKey = ""
	}
	if err != nil {
		http.Error(w, "Could not read backup settings", http.StatusInternalServerError)
		return
	}
	_ = backupPage(data, s.csrf(r)).Render(ctx, w)
}

func backupForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid backup form", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleBackupPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) || !backupForm(w, r) {
		return
	}
	svc, ok := s.backupService(w, r)
	if !ok {
		return
	}
	keep, _ := strconv.Atoi(r.PostForm.Get("keep"))
	p := store.BackupPolicy{
		Schedule: strings.TrimSpace(r.PostForm.Get("schedule")), Keep: keep,
		LocalOnly: r.PostForm.Has("local_only"), SkipPostgres: r.PostForm.Has("skip_postgres"), StopApp: r.PostForm.Has("stop_app"),
		Quiesce: r.PostForm.Get("quiesce"), Resume: r.PostForm.Get("resume"),
		Export: r.PostForm.Get("export"), Import: r.PostForm.Get("import"),
	}
	for _, line := range strings.Split(r.PostForm.Get("sources"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kind, path, _ := strings.Cut(line, " ")
		p.Sources = append(p.Sources, store.BackupSource{Type: kind, Path: strings.TrimSpace(path)})
	}
	if err := s.backups.manager.SavePolicy(r.Context(), *svc, p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, serviceBackupsURL(svc.Name), http.StatusSeeOther)
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	svc, ok := s.backupService(w, r)
	if !ok {
		return
	}
	if _, err := s.backups.manager.Queue(r.Context(), *svc); err != nil {
		http.Error(w, "Could not queue backup", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, serviceBackupsURL(svc.Name), http.StatusSeeOther)
}

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}
	s.serveBackupArchive(w, r, job)
}

func (s *Server) serveBackupArchive(w http.ResponseWriter, r *http.Request, job store.BackupJob) {
	f, err := s.backups.manager.Open(job)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	name := fmt.Sprintf("nori-backup-%d.nori", job.ID)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, job.Created, f)
}

func (s *Server) handleBackupKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	key := s.backups.manager.RecoveryKey()
	if key == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="nori-recovery-key.txt"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, key)
}

func (s *Server) handleBackupRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}
	if err := s.backups.manager.RetryUpload(r.Context(), job); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/backups", http.StatusSeeOther)
}

func (s *Server) handleS3Config(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	w.Header().Add("Vary", "Accept")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		s.renderS3SaveError(w, r, http.StatusBadRequest, backup.S3Config{}, "Could not read the S3 settings form. Try again.", "")
		return
	}
	f := r.PostForm
	c := backup.S3Config{Bucket: strings.TrimSpace(f.Get("bucket")), Region: strings.TrimSpace(f.Get("region")),
		Endpoint: strings.TrimSpace(f.Get("endpoint")), VirtualHosted: f.Has("virtual_hosted"),
		Prefix: strings.TrimSpace(f.Get("prefix")), AccessKeyID: strings.TrimSpace(f.Get("access_key_id")), SecretAccessKey: strings.TrimSpace(f.Get("secret_access_key")),
		Disabled: f.Has("disabled"), ReadOnly: f.Has("read_only")}
	if err := s.backups.manager.SaveS3(r.Context(), c); err != nil {
		var fieldErr *backup.S3FieldError
		if errors.As(err, &fieldErr) {
			s.renderS3SaveError(w, r, http.StatusBadRequest, c, fieldErr.Message, fieldErr.Field)
		} else {
			s.renderS3SaveError(w, r, http.StatusInternalServerError, c, "Could not save S3 settings. Try again.", "")
		}
		return
	}
	const location = "/backups?s3_saved=1#s3-storage"
	if r.Header.Get("Accept") == "application/json" {
		writeS3SaveResponse(w, http.StatusOK, s3SaveResponse{Location: location})
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

type s3SaveResponse struct {
	Message  string `json:"message,omitempty"`
	Field    string `json:"field,omitempty"`
	Location string `json:"location,omitempty"`
}

func writeS3SaveResponse(w http.ResponseWriter, status int, response s3SaveResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) renderS3SaveError(w http.ResponseWriter, r *http.Request, status int, c backup.S3Config, message, field string) {
	if r.Header.Get("Accept") == "application/json" {
		writeS3SaveResponse(w, status, s3SaveResponse{Message: message, Field: field})
		return
	}
	if c.SecretAccessKey != "" {
		message += " Re-enter the secret access key before trying again."
		c.SecretAccessKey = ""
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = backupS3SettingsErrorPage(c, s.csrf(r), message, field).Render(r.Context(), w)
}

func s3FieldAttributes(field, errorField string) templ.Attributes {
	help := "s3-help-" + field
	if field == errorField {
		return templ.Attributes{"aria-describedby": help + " s3-save-error", "aria-invalid": "true", "autofocus": true}
	}
	return templ.Attributes{"aria-describedby": help}
}

func (s *Server) handleBackupDiskPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) || !backupForm(w, r) {
		return
	}
	var values [4]int64
	for i, field := range []string{"warning_percent", "warning_gib", "critical_percent", "critical_gib"} {
		n, err := strconv.ParseInt(r.PostForm.Get(field), 10, 32)
		if err != nil || n < 0 {
			http.Error(w, "Invalid disk threshold", http.StatusBadRequest)
			return
		}
		values[i] = n
	}
	p := backup.DiskPolicy{WarningPercent: values[0], WarningBytes: values[1] * backup.GiB,
		CriticalPercent: values[2], CriticalBytes: values[3] * backup.GiB}
	for _, path := range strings.Split(r.PostForm.Get("paths"), "\n") {
		if path := strings.TrimSpace(path); path != "" {
			p.Paths = append(p.Paths, path)
		}
	}
	if err := s.backups.manager.SaveDiskPolicy(r.Context(), p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/backups#disk-space", http.StatusSeeOther)
}

func (s *Server) handleS3Objects(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	objects, next, err := s.backups.manager.S3Objects(ctx, r.URL.Query().Get("cursor"))
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = backupS3Page(nil, "", s.csrf(r), "Could not browse remote backups. Check that S3 access is enabled and verify the endpoint, region, bucket and credentials. The key needs permission to list objects under the backup prefix.").Render(r.Context(), w)
		return
	}
	_ = backupS3Page(objects, next, s.csrf(r), "").Render(r.Context(), w)
}

func (s *Server) handleS3Import(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) || !backupForm(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	file, err := s.backups.manager.OpenS3(ctx, r.PostForm.Get("key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer file.Close()
	s.stageImport(w, r.WithContext(ctx), file, r.PostForm.Get("identity"))
}

func (s *Server) handleLocalImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) || !backupForm(w, r) {
		return
	}
	job, ok := s.backupJob(w, r)
	if !ok {
		return
	}
	f, err := s.backups.manager.Open(job)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	s.stageImport(w, r, f, r.PostForm.Get("identity"))
}

// Read the CSRF token and optional identity before the upload. The generic
// form middleware would otherwise buffer multipart files in system temp.
func (s *Server) handleBackupUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	// Renew the deadline on each read so slow, progressing uploads can finish.
	controller := http.NewResponseController(w)
	defer controller.SetReadDeadline(time.Time{})
	r.Body = &backupUploadBody{ReadCloser: r.Body, controller: controller}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<40)
	multipart, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Choose a backup file", http.StatusBadRequest)
		return
	}
	token, err := shortImportField(multipart, "csrf_token")
	if err != nil || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf(r))) != 1 {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	identity, err := shortImportField(multipart, "identity")
	if err != nil {
		http.Error(w, "Invalid recovery key field", http.StatusBadRequest)
		return
	}
	part, err := multipart.NextPart()
	if err != nil || part.FormName() != "archive" || part.FileName() == "" {
		http.Error(w, "Choose a backup file", http.StatusBadRequest)
		return
	}
	s.stageImport(w, r, part, identity)
}

func shortImportField(reader *multipart.Reader, name string) (string, error) {
	part, err := reader.NextPart()
	if err != nil {
		return "", err
	}
	if part.FormName() != name {
		return "", errors.New("unexpected import field")
	}
	data, err := io.ReadAll(io.LimitReader(part, 513))
	if err != nil || len(data) > 512 {
		return "", errors.New("invalid import field")
	}
	return string(data), nil
}

func (s *Server) stageImport(w http.ResponseWriter, r *http.Request, src io.Reader, key string) {
	draft, err := s.backups.stageDraft(r.Context(), src, key, "")
	if err != nil {
		http.Error(w, "Could not open backup. Check the recovery key, file integrity and available disk space. "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/backups/restore/"+draft.token, http.StatusSeeOther)
}

func (p *backupPages) stageDraft(ctx context.Context, src io.Reader, key, owner string) (*restoreDraft, error) {
	manifest, directory, cleanup, err := p.manager.DecodeImport(ctx, src, key)
	if err != nil {
		return nil, err
	}
	draft := &restoreDraft{token: rand.Text(), owner: owner, manifest: manifest, directory: directory, expires: time.Now().Add(30 * time.Minute), cleanup: cleanup}
	p.mu.Lock()
	p.draft = draft
	p.mu.Unlock()
	time.AfterFunc(time.Until(draft.expires), func() {
		if expired := p.takeDraft(draft.token, owner); expired != nil {
			expired.cleanup()
		}
	})
	return draft, nil
}

func (p *backupPages) takeDraft(token, owner string) *restoreDraft {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draft == nil || p.draft.token != token || p.draft.owner != owner {
		return nil
	}
	draft := p.draft
	p.draft = nil
	return draft
}

func (p *backupPages) returnDraft(draft *restoreDraft) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draft != nil || !time.Now().Before(draft.expires) {
		return false
	}
	p.draft = draft
	return true
}

func (s *Server) handleRestoreReview(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) {
		return
	}
	p := s.backups
	p.mu.Lock()
	draft := p.draft
	p.mu.Unlock()
	if draft == nil || draft.token != chi.URLParam(r, "token") || draft.owner != "" || !time.Now().Before(draft.expires) {
		http.NotFound(w, r)
		return
	}
	options := backup.RestoreOptions{Name: draft.manifest.Service.Name, DeployScript: &draft.manifest.Service.DeployScript, Targets: make(map[int]string)}
	for i, source := range draft.manifest.Policy.Sources {
		options.Targets[i] = source.Path
	}
	_ = backupReviewPage(draft.manifest, options, draft.token, s.csrf(r), "", "").Render(r.Context(), w)
}

func (s *Server) handleRestoreConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w, r) || !backupForm(w, r) {
		return
	}
	draft := s.backups.takeDraft(chi.URLParam(r, "token"), "")
	if draft == nil {
		http.NotFound(w, r)
		return
	}
	keepDraft := false
	defer func() {
		if !keepDraft {
			draft.cleanup()
		}
	}()
	if r.PostForm.Has("cancel") {
		http.Redirect(w, r, "/backups", http.StatusSeeOther)
		return
	}
	options := backup.RestoreOptions{Name: strings.TrimSpace(r.PostForm.Get("name")),
		Network: strings.TrimSpace(r.PostForm.Get("network")), Domain: strings.TrimSpace(r.PostForm.Get("domain")),
		Targets: make(map[int]string), ReviewCommands: r.PostForm.Has("reviewed")}
	if r.PostForm.Has("deploy_script") {
		script := r.PostForm.Get("deploy_script")
		options.DeployScript = &script
	}
	for i := range draft.manifest.Policy.Sources {
		options.Targets[i] = strings.TrimSpace(r.PostForm.Get("target_" + strconv.Itoa(i)))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	svc, err := s.backups.manager.Restore(ctx, draft.manifest, draft.directory, options)
	if err != nil {
		// Retry only when the attempt stopped before allocating a service.
		// Once writes have started, require a new review of the destination.
		if svc.ID == 0 && s.backups.returnDraft(draft) {
			keepDraft = true
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			var fieldErr *backup.RestoreFieldError
			field := ""
			if errors.As(err, &fieldErr) {
				field = fieldErr.Field
			}
			_ = backupReviewPage(draft.manifest, options, draft.token, s.csrf(r), err.Error(), field).Render(r.Context(), w)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/services/"+url.PathEscape(svc.Name), http.StatusSeeOther)
}

func backupSources(sources []store.BackupSource) string {
	var lines []string
	for _, source := range sources {
		lines = append(lines, source.Type+" "+source.Path)
	}
	return strings.Join(lines, "\n")
}

func restoreErrorAttributes(errorField, field string) templ.Attributes {
	if errorField != field {
		return nil
	}
	return templ.Attributes{"aria-invalid": "true", "aria-describedby": "restore-error", "autofocus": true}
}

func backupUploadStatus(status string) string {
	if status == "" {
		return "—"
	}
	return status
}

type backupUploadBody struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (body *backupUploadBody) Read(p []byte) (int, error) {
	if err := body.controller.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	return body.ReadCloser.Read(p)
}
