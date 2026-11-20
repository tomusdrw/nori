package backup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"nori/internal/store"
)

func TestCustomS3EndpointListAndRead(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, nil)
	archive := "testing/instance/app/backup.nori"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.Header.Get("Authorization"), "Credential=example/") {
			t.Errorf("unexpected S3 request %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/dev.backups":
			if r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("prefix") != "testing/" {
				t.Errorf("unexpected list query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, "<ListBucketResult><Contents><Key>%s</Key></Contents></ListBucketResult>", archive)
		case "/dev.backups/" + archive:
			_, _ = io.WriteString(w, "encrypted archive")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "dev.backups", Prefix: "testing", AccessKeyID: "example", SecretAccessKey: "secret"}
	if err := m.SaveS3(ctx, c); err != nil {
		t.Fatal(err)
	}
	objects, _, err := m.S3Objects(ctx, "")
	if err != nil || len(objects) != 1 || objects[0] != archive {
		t.Fatalf("list from configured endpoint: %v %v", objects, err)
	}
	reader, err := m.OpenS3(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "encrypted archive" {
		t.Fatalf("read from configured endpoint: %v", err)
	}
}

func TestS3UploadFailureCanBeRetriedWithoutRecreatingArchive(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t, nil)
	svc := &store.Service{Name: "app", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript, DeployScript: "true"}
	if err := st.SaveServiceConfig(ctx, svc, nil, nil); err != nil {
		t.Fatal(err)
	}
	c := S3Config{Endpoint: "https://s3.example.test", Region: "us-west-2", Bucket: "private-backups", Prefix: "testing", AccessKeyID: "example", SecretAccessKey: "secret"}
	if err := m.SaveS3(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Queue(ctx, *svc); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.BackupsByStatus(ctx, "queued")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("queue: %v %v", jobs, err)
	}
	m.capture(ctx, jobs[0])
	job, err := st.BackupJob(ctx, jobs[0].ID)
	if err != nil || job.Status != "complete" || job.UploadStatus != "pending" {
		t.Fatalf("created backup: %v %v", job, err)
	}
	archive, err := os.ReadFile(m.archivePath(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/private-backups/testing/") ||
			!strings.Contains(r.URL.Path, "/app/") || !strings.Contains(r.Header.Get("Authorization"), "Credential=example/") {
			t.Errorf("unexpected S3 request %s %s", r.Method, r.URL.Path)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != string(archive) {
			t.Errorf("upload doesn't match local archive: %v", err)
		}
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"etag"`)
	}))
	defer server.Close()
	c.Endpoint = server.URL
	client := c.client(func(options *s3.Options) {
		options.Retryer = aws.NopRetryer{}
	})
	m.uploadJob(ctx, client, c, job)
	job, err = st.BackupJob(ctx, job.ID)
	if err != nil || job.UploadStatus != "retry" || job.Status != "complete" {
		t.Fatalf("failed upload incorrectly changed local availability: %v %v", job, err)
	}
	fail.Store(false)
	if err := m.RetryUpload(ctx, job); err != nil {
		t.Fatal(err)
	}
	job, err = st.BackupJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.uploadJob(ctx, client, c, job)
	job, err = st.BackupJob(ctx, job.ID)
	if err != nil || job.UploadStatus != "uploaded" {
		t.Fatalf("retry didn't upload the same archive: %v %v", job, err)
	}
}

func TestRecoverUploadOutcomeWithoutSendingAgain(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t, nil)
	svc := &store.Service{Name: "app", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript, DeployScript: "true"}
	if err := st.SaveServiceConfig(ctx, svc, nil, nil); err != nil {
		t.Fatal(err)
	}
	if queued, err := m.Queue(ctx, *svc); !queued || err != nil {
		t.Fatalf("queue: %v", err)
	}
	jobs, err := st.BackupsByStatus(ctx, "queued")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("queued jobs: %v, %v", jobs, err)
	}
	job := jobs[0]
	if err := st.SetBackupStatus(ctx, job.ID, "complete", "", 1, "uploading"); err != nil {
		t.Fatal(err)
	}
	outcome := uploadOutcome{ID: job.ID, ServiceID: svc.ID, Status: "uploaded", Attempts: 1}
	if err := m.writeJournal(fmt.Sprintf("upload-result-%d", job.ID), outcome); err != nil {
		t.Fatal(err)
	}
	m.reconcile(ctx)
	got, err := st.BackupJob(ctx, job.ID)
	if err != nil || got.UploadStatus != "uploaded" || got.UploadAttempts != 1 {
		t.Fatalf("result not recovered: %+v %v", got, err)
	}
}
