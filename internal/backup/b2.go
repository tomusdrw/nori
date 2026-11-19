package backup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"nori/internal/store"
)

const b2Setting = "backup_b2"

var b2Region = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
var b2Bucket = regexp.MustCompile(`^[a-zA-Z0-9-]{6,63}$`)
var b2Prefix = regexp.MustCompile(`^[a-zA-Z0-9_/-]{1,200}$`)

type B2Config struct {
	Region   string
	Bucket   string
	Prefix   string
	KeyID    string
	AppKey   string
	Disabled bool
	ReadOnly bool
}

func (c B2Config) Enabled() bool { return !c.Disabled && c.Bucket != "" }

func (c B2Config) UploadEnabled() bool { return c.Enabled() && !c.ReadOnly }

func (c B2Config) client(options ...func(*s3.Options)) *s3.Client {
	return s3.NewFromConfig(aws.Config{
		Region:                     c.Region,
		Credentials:                credentials.NewStaticCredentialsProvider(c.KeyID, c.AppKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://s3." + c.Region + ".backblazeb2.com")
		o.UsePathStyle = true
		for _, configure := range options {
			configure(o)
		}
	})
}

func (m *Manager) B2(ctx context.Context) (B2Config, error) {
	c := B2Config{Prefix: "nori"}
	raw, err := m.store.ReadBackupSecret(ctx, b2Setting)
	if errors.Is(err, store.ErrNotFound) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(raw), &c)
}

func (m *Manager) SaveB2(ctx context.Context, c B2Config) error {
	c.Prefix = strings.Trim(c.Prefix, "/")
	if c.Bucket != "" {
		if c.AppKey == "" {
			old, err := m.B2(ctx)
			if err != nil {
				return err
			}
			if old.KeyID == c.KeyID && old.Bucket == c.Bucket && old.Region == c.Region {
				c.AppKey = old.AppKey
			}
		}
		if !b2Region.MatchString(c.Region) || !b2Bucket.MatchString(c.Bucket) ||
			!b2Prefix.MatchString(c.Prefix) || c.KeyID == "" || c.AppKey == "" {
			return errors.New("provide a valid B2 region, bucket, prefix and application key")
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return m.store.WriteBackupSecret(ctx, b2Setting, string(raw))
}

func (m *Manager) B2Objects(ctx context.Context, cursor string) ([]string, string, error) {
	c, err := m.B2(ctx)
	if err != nil || !c.Enabled() {
		return nil, "", errors.New("Backblaze B2 is unavailable")
	}
	req := &s3.ListObjectsV2Input{Bucket: aws.String(c.Bucket), Prefix: aws.String(c.Prefix + "/"), MaxKeys: aws.Int32(100)}
	if cursor != "" {
		req.ContinuationToken = &cursor
	}
	page, err := c.client().ListObjectsV2(ctx, req)
	if err != nil {
		return nil, "", errors.New("could not list B2 backups")
	}
	var objects []string
	for _, object := range page.Contents {
		if key := aws.ToString(object.Key); strings.HasSuffix(key, ".nori") {
			objects = append(objects, key)
		}
	}
	return objects, aws.ToString(page.NextContinuationToken), nil
}

func (m *Manager) OpenB2(ctx context.Context, key string) (io.ReadCloser, error) {
	c, err := m.B2(ctx)
	if err != nil || !c.Enabled() || !strings.HasPrefix(key, c.Prefix+"/") || !strings.HasSuffix(key, ".nori") {
		return nil, errors.New("invalid B2 backup selection or storage is unavailable")
	}
	object, err := c.client().GetObject(ctx, &s3.GetObjectInput{Bucket: &c.Bucket, Key: &key})
	if err != nil {
		return nil, errors.New("could not read B2 backup")
	}
	return object.Body, nil
}

func (m *Manager) RetryUpload(ctx context.Context, job store.BackupJob) error {
	c, err := m.B2(ctx)
	if err != nil || !c.UploadEnabled() || job.Status != "complete" || job.UploadStatus == "uploading" {
		return errors.New("a complete local backup and enabled B2 storage are required")
	}
	return m.store.SetBackupUpload(ctx, job.ID, "pending", 0, 0)
}

func (m *Manager) runUploads(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		c, err := m.B2(ctx)
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

func (m *Manager) uploadJob(parent context.Context, client *s3.Client, c B2Config, job store.BackupJob) {
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
	if parent.Err() != nil {
		return
	}
	status, attempts, next := "uploaded", job.UploadAttempts+1, int64(0)
	if err != nil {
		status = "retry"
		next = time.Now().Add(time.Minute * (1 << min(attempts-1, 6))).Unix()
		if attempts >= 8 {
			status = "failed"
		}
	}
	if err := m.store.SetBackupUpload(parent, job.ID, status, attempts, next); err != nil {
		log.Printf("backup: save upload status: %v", err)
		return
	}
	m.prune(parent, job.ServiceID)
}

func (m *Manager) startUpload(ctx context.Context, id int64) (*os.File, error) {
	m.archiveMu.Lock()
	defer m.archiveMu.Unlock()
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
