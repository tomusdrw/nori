package backup

import (
	"context"
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

func TestB2UploadFailureCanBeRetriedWithoutRecreatingArchive(t *testing.T) {
	ctx := context.Background()
	m, st := newTestManager(t, nil)
	svc := &store.Service{Name: "app", Policy: store.PolicyManual, DeploymentMode: store.DeploymentModeManualScript, DeployScript: "true"}
	if err := st.SaveServiceConfig(ctx, svc, nil, nil); err != nil {
		t.Fatal(err)
	}
	c := B2Config{Region: "us-west-004", Bucket: "private-backups", Prefix: "testing", KeyID: "example", AppKey: "secret"}
	if err := m.SaveB2(ctx, c); err != nil {
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
	client := c.client(func(options *s3.Options) {
		options.BaseEndpoint = &server.URL
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
