package docker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

func TestResourceSDKInventoryAndStats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/v1.47")
		var body string
		switch path {
		case "/volumes":
			body = `{"Volumes":[{"Name":"local"},{"Name":"plugin"}]}`
		case "/system/df":
			if r.URL.Query().Get("type") != "volume" {
				t.Errorf("disk usage query %s", r.URL)
			}
			body = `{"Volumes":[{"Name":"local","UsageData":{"Size":0,"RefCount":1}}]}`
		case "/containers/json":
			if r.URL.Query().Get("all") != "1" {
				t.Errorf("container inventory excludes stopped: %s", r.URL)
			}
			body = `[{"Id":"a","Names":["/api"],"State":"running"}]`
		case "/containers/a/json":
			body = `{"Id":"a","State":{"Running":true,"StartedAt":"2026-09-25T12:00:00Z"},"SizeRw":0,"LogPath":"/daemon/log","HostConfig":{"Memory":256,"LogConfig":{"Type":"json-file"}}}`
		case "/containers/unlimited/json", "/containers/unknown/json":
			body = `{"State":{"Running":true,"StartedAt":"2026-09-25T12:00:00Z"},"HostConfig":{}}`
			if path == "/containers/unknown/json" {
				body = `{"State":{"Running":true,"StartedAt":"2026-09-25T12:00:00Z"}}`
			}
		case "/containers/a/stats", "/containers/unlimited/stats", "/containers/unknown/stats":
			if r.URL.Query().Get("one-shot") != "1" && r.URL.Query().Get("one-shot") != "true" {
				t.Errorf("streaming stats: %s", r.URL)
			}
			body = `{"read":"2026-09-25T12:00:05Z","cpu_stats":{"cpu_usage":{"total_usage":200}},"memory_stats":{"usage":500,"limit":1000,"stats":{"inactive_file":100}},"networks":{"eth0":{"rx_bytes":300,"tx_bytes":10},"eth1":{"rx_bytes":200,"tx_bytes":4}},"pids_stats":{"current":2},"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read","value":10},{"op":"WRITE","value":30},{"op":"Total","value":40}]}}`
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	dk := realClient{cli: cli}
	ctx := t.Context()
	vols, err := dk.ResourceVolumes(ctx)
	if err != nil || len(vols) != 2 || vols[0].UsageData == nil || vols[0].UsageData.Size != 0 || vols[1].UsageData != nil {
		t.Fatalf("volumes %v %v", vols, err)
	}
	cs, err := dk.ResourceContainers(ctx)
	if err != nil || len(cs) != 1 {
		t.Fatalf("containers %v %v", cs, err)
	}
	disk, err := dk.ResourceStorage(ctx, "a")
	if err != nil || disk.Writable == nil || *disk.Writable != 0 || disk.LogDriver != "json-file" || disk.LogPath != "/daemon/log" {
		t.Fatalf("storage %+v %v", disk, err)
	}
	s, err := dk.ResourceStats(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range map[string]uint64{"rx": 500, "tx": 14, "read": 10, "write": 30, "cpu": 200} {
		if s.Counters[k] != n {
			t.Errorf("%s=%d, want %d", k, s.Counters[k], n)
		}
	}
	if s.Values["memory"] != 400 || s.Values["limit"] != 256 || s.Values["pids"] != 2 {
		t.Errorf("gauges %v", s.Values)
	}
	for _, id := range []string{"unlimited", "unknown"} {
		s, err := dk.ResourceStats(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := s.Values["limit"]; ok {
			t.Fatalf("%s reported host memory as a quota: %v", id, s.Values)
		}
		if _, ok := s.Values["unlimited"]; ok != (id == "unlimited") {
			t.Fatalf("%s: %v", id, s.Values)
		}
	}

}

func TestResourceStatsOmittedFields(t *testing.T) {
	now := time.Now().UTC()
	b, _ := json.Marshal(map[string]any{"read": now, "memory_stats": map[string]any{"usage": 0}})
	s, err := decodeRuntimeStats(strings.NewReader(string(b)), now)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := s.Values["memory"]; !ok || n != 0 || len(s.Values) != 1 || len(s.Counters) != 0 {
		t.Fatalf("missing inferred as zero: %+v", s)
	}
	if _, err := decodeRuntimeStats(strings.NewReader("{}"), time.Time{}); err == nil {
		t.Fatal("accepted empty Docker stats")
	}
}

var _ ResourceClient = (*realClient)(nil)
