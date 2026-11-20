package backup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"nori/internal/store"
)

const s3Setting = "backup_s3"

var s3Region = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
var s3Bucket = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{1,61}[a-zA-Z0-9]$`)
var s3Prefix = regexp.MustCompile(`^[a-zA-Z0-9_/-]{1,200}$`)

type S3Config struct {
	Endpoint        string
	VirtualHosted   bool
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	Disabled        bool
	ReadOnly        bool
}

type S3FieldError struct {
	Field   string
	Message string
}

func (e *S3FieldError) Error() string { return e.Message }

func (c S3Config) Enabled() bool { return !c.Disabled && c.Bucket != "" && c.Endpoint != "" }

func (c S3Config) UploadEnabled() bool { return c.Enabled() && !c.ReadOnly }

func (c S3Config) client(options ...func(*s3.Options)) *s3.Client {
	return s3.NewFromConfig(aws.Config{
		Region:                     c.Region,
		Credentials:                credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(c.Endpoint)
		o.UsePathStyle = !c.VirtualHosted
		for _, configure := range options {
			configure(o)
		}
	})
}

func (m *Manager) S3(ctx context.Context) (S3Config, error) {
	c := S3Config{Prefix: "nori"}
	raw, err := m.store.ReadBackupSecret(ctx, s3Setting)
	if errors.Is(err, store.ErrNotFound) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(raw), &c)
}

func (m *Manager) SaveS3(ctx context.Context, c S3Config) error {
	c.Prefix = strings.Trim(c.Prefix, "/")
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
			u.Hostname() == "" || u.User != nil || strings.ContainsAny(c.Endpoint, "?#") {
			return &S3FieldError{"endpoint", "Enter an HTTP or HTTPS S3 service URL without credentials, a query string or a fragment."}
		}
		c.Endpoint = strings.TrimRight(c.Endpoint, "/")
	}
	if c.Bucket != "" {
		switch {
		case c.Endpoint == "":
			return &S3FieldError{"endpoint", "Enter the S3 service endpoint for this bucket."}
		case !s3Bucket.MatchString(c.Bucket):
			return &S3FieldError{"bucket", "Enter a 3–63 character bucket name. Start and end with a letter or number. Dots and hyphens are allowed inside the name."}
		case !s3Region.MatchString(c.Region):
			return &S3FieldError{"region", "Enter the signing region from your storage provider, such as us-west-2."}
		case !s3Prefix.MatchString(c.Prefix):
			return &S3FieldError{"prefix", "Enter an object prefix such as nori, using up to 200 letters, numbers, underscores, hyphens or slashes."}
		case c.AccessKeyID == "":
			return &S3FieldError{"access_key_id", "Enter the access key ID from your storage provider."}
		}
		if c.SecretAccessKey == "" {
			old, err := m.S3(ctx)
			if err != nil {
				return err
			}
			if old.AccessKeyID == c.AccessKeyID && old.Bucket == c.Bucket && old.Region == c.Region &&
				old.Endpoint == c.Endpoint && old.VirtualHosted == c.VirtualHosted {
				c.SecretAccessKey = old.SecretAccessKey
			}
		}
		if c.SecretAccessKey == "" {
			return &S3FieldError{"secret_access_key", "Enter the secret access key for this destination and access key ID."}
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return m.store.WriteBackupSecret(ctx, s3Setting, string(raw))
}

func (m *Manager) S3Objects(ctx context.Context, cursor string) ([]string, string, error) {
	c, err := m.S3(ctx)
	if err != nil || !c.Enabled() {
		return nil, "", errors.New("S3 backup storage is unavailable")
	}
	req := &s3.ListObjectsV2Input{Bucket: aws.String(c.Bucket), Prefix: aws.String(c.Prefix + "/"), MaxKeys: aws.Int32(100)}
	if cursor != "" {
		req.ContinuationToken = &cursor
	}
	page, err := c.client().ListObjectsV2(ctx, req)
	if err != nil {
		return nil, "", errors.New("could not list remote backups")
	}
	var objects []string
	for _, object := range page.Contents {
		if key := aws.ToString(object.Key); strings.HasSuffix(key, ".nori") {
			objects = append(objects, key)
		}
	}
	return objects, aws.ToString(page.NextContinuationToken), nil
}

func (m *Manager) OpenS3(ctx context.Context, key string) (io.ReadCloser, error) {
	c, err := m.S3(ctx)
	if err != nil || !c.Enabled() || !strings.HasPrefix(key, c.Prefix+"/") || !strings.HasSuffix(key, ".nori") {
		return nil, errors.New("invalid remote backup selection or storage is unavailable")
	}
	object, err := c.client().GetObject(ctx, &s3.GetObjectInput{Bucket: &c.Bucket, Key: &key})
	if err != nil {
		return nil, errors.New("could not read remote backup")
	}
	return object.Body, nil
}

func (m *Manager) RetryUpload(ctx context.Context, job store.BackupJob) error {
	c, err := m.S3(ctx)
	if err != nil || !c.UploadEnabled() || job.Status != "complete" || job.UploadStatus == "uploading" {
		return errors.New("a complete local backup and enabled S3 storage are required")
	}
	policy, _, err := m.store.BackupPolicy(ctx, job.ServiceID)
	if err != nil {
		return err
	}
	if policy.LocalOnly {
		return errors.New("service only uses local backups")
	}
	m.archiveMu.Lock()
	defer m.archiveMu.Unlock()
	if _, err := os.Stat(m.archivePath(job.ID)); err != nil {
		return errors.New("local backup archive is unavailable")
	}
	return m.store.RequeueBackupUpload(ctx, job.ID)
}

func (m *Manager) runUploads(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		m.finishUploads(ctx)
		c, err := m.S3(ctx)
		if err == nil && c.UploadEnabled() {
			jobs, err := m.store.PendingUploads(ctx)
			if err != nil {
				log.Printf("backup: load uploads: %v", err)
			}
			client := c.client()
			for _, job := range jobs {
				if ctx.Err() != nil {
					break
				}
				m.uploadJob(ctx, client, c, job)
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (m *Manager) uploadJob(parent context.Context, client *s3.Client, c S3Config, job store.BackupJob) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Hour)
	defer cancel()
	file, err := m.startUpload(ctx, job.ID)
	if err != nil {
		return
	}
	defer file.Close()
	// Use an instance-specific component so two installations sharing one
	// prefix cannot overwrite each other's backups.
	id := sha256.Sum256([]byte(m.instanceID))
	service := job.ServiceName
	if !restoredServiceName.MatchString(service) {
		service = fmt.Sprintf("service-%d", job.ServiceID)
	}
	key := fmt.Sprintf("%s/%x/%s/%s-%d.nori", c.Prefix, id[:12], service, job.Created.UTC().Format("20060102T150405Z"), job.ID)
	uploader := transfermanager.New(client, func(o *transfermanager.Options) {
		o.PartSizeBytes = 128 << 20
		o.Concurrency = 2
		o.FailTimeout = 30 * time.Second
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	_, err = uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: &c.Bucket, Key: &key, Body: file})
	status, attempts, next := "uploaded", job.UploadAttempts+1, int64(0)
	if err != nil {
		status = "retry"
		next = time.Now().Add(time.Minute * (1 << min(attempts-1, 6))).Unix()
		if attempts >= 8 {
			status = "failed"
		}
	}
	outcome := uploadOutcome{job.ID, job.ServiceID, status, attempts, next}
	name := fmt.Sprintf("upload-result-%d", job.ID)
	if m.uploadResults == nil {
		m.uploadResults = make(map[string]uploadOutcome)
	}
	m.uploadResults[name] = outcome
	if err := m.writeJournal(name, outcome); err != nil {
		log.Printf("backup: save upload recovery record: %v", err)
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer cancel()
	m.finishUpload(finish, name, outcome)
}

type uploadOutcome struct {
	ID, ServiceID int64
	Status        string
	Attempts      int
	Next          int64
}

func (m *Manager) finishUploads(ctx context.Context) {
	files, err := os.ReadDir(m.dir)
	if err != nil {
		log.Printf("backup: list upload recovery records: %v", err)
	}
	if m.uploadResults == nil {
		m.uploadResults = make(map[string]uploadOutcome)
	}
	for _, file := range files {
		name := file.Name()
		if !strings.HasPrefix(name, "upload-result-") {
			continue
		}
		if _, exists := m.uploadResults[name]; exists {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, name))
		var outcome uploadOutcome
		if err == nil {
			err = json.Unmarshal(data, &outcome)
		}
		if err != nil {
			log.Printf("backup: read upload recovery record %s: %v", name, err)
			continue
		}
		m.uploadResults[name] = outcome
	}
	for name, outcome := range m.uploadResults {
		m.finishUpload(ctx, name, outcome)
	}
}

func (m *Manager) finishUpload(ctx context.Context, name string, outcome uploadOutcome) {
	if err := m.store.SetBackupUpload(ctx, outcome.ID, outcome.Status, outcome.Attempts, outcome.Next); err != nil {
		log.Printf("backup: save upload status: %v", err)
		return
	}
	if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("backup: remove upload recovery record: %v", err)
		return
	}
	delete(m.uploadResults, name)
	m.prune(ctx, outcome.ServiceID)
}

func (m *Manager) startUpload(ctx context.Context, id int64) (*os.File, error) {
	m.archiveMu.Lock()
	defer m.archiveMu.Unlock()
	if _, recorded := m.uploadResults[fmt.Sprintf("upload-result-%d", id)]; recorded {
		return nil, errors.New("upload completion is pending")
	}
	job, err := m.store.BackupJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if job.UploadStatus != "pending" && job.UploadStatus != "retry" {
		return nil, errors.New("backup is no longer pending upload")
	}
	policy, _, err := m.store.BackupPolicy(ctx, job.ServiceID)
	if err != nil {
		return nil, err
	}
	if policy.LocalOnly {
		err := m.store.SetBackupUpload(ctx, id, "local only", job.UploadAttempts, 0)
		return nil, errors.Join(errors.New("service only uses local backups"), err)
	}
	file, err := m.Open(job)
	if err != nil {
		_ = m.store.SetBackupUpload(ctx, id, "failed", job.UploadAttempts, 0)
		return nil, err
	}
	if err := m.store.SetBackupUpload(ctx, id, "uploading", job.UploadAttempts, 0); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
