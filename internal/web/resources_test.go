package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"nori/internal/store"
)

func TestResourcePagesAndLimits(t *testing.T) {
	srv, st := mcpSettingsServer(t)
	svc := &store.Service{Name: "library", Policy: store.PolicyManual, WatchedImage: "library"}
	if err := st.CreateService(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, srv)
	path := "/services/library/resources"
	for _, target := range []string{"/resources", path} {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound {
			t.Errorf("unauthenticated GET %s returned %d", target, rr.Code)
		}
	}
	index := getAuthed(t, srv, cookies, "/resources")
	for _, want := range []string{path, "Storage sources", "Unavailable"} {
		if !strings.Contains(index, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	page := getAuthed(t, srv, cookies, path)
	for _, want := range []string{"CPU", "Memory", "Storage", "Containers", "Usage threshold", "Growth threshold"} {
		if !strings.Contains(page, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	postAuthed(t, srv, cookies, path, url.Values{"csrf_token": {csrfFromBody(page)}, "usage_bytes": {"1048576"}, "growth_bytes": {"0.5 KiB"}, "threshold_cpu": {"30.5"}, "directories": {"/srv/reports\n/srv/media"}}.Encode())
	saved := getAuthed(t, srv, cookies, path)
	for _, value := range []string{"value=\"1 MiB\"", "value=\"512 B\"", "value=\"30.5\"", "/srv/reports", "/srv/media"} {
		if !strings.Contains(saved, value) {
			t.Errorf("saved response missing %s", value)
		}
	}
}

func TestResourceComparisonsAndChartGaps(t *testing.T) {
	now := time.Now()
	add := func(key, kind string, owners []int64, value float64) store.ResourceReading {
		return store.ResourceReading{Key: key, Name: key, Kind: kind, State: "available", Services: owners, Values: map[string]float64{"bytes": value}}
	}
	readings := []store.ResourceReading{
		add("shared", "volume", []int64{1, 2}, 100),
		add("private", "directory", []int64{1}, 20),
		add("unowned", "volume", nil, 8),
		{Key: "db", Kind: "postgres-database", Services: []int64{1}, State: "available", Values: map[string]float64{"logical": 1000}},
		{Key: "log", Kind: "logs", Services: []int64{1}, State: "unavailable"},
	}
	current := store.ResourceSnapshot{At: now, Readings: readings}
	day := store.ResourceSnapshot{At: now.Add(-24 * time.Hour), Readings: slices.Clone(readings)}
	day.Readings[0] = add("shared", "volume", []int64{1, 2}, 80)
	view := newResourceView(resourceSamples{storage: current, day: day}, nil, now)
	for _, tc := range []struct {
		id        int64
		key, want string
	}{
		{-1, "bytes", "128 B (partial)"}, {1, "bytes", "120 B (partial)"}, {2, "bytes", "100 B"},
		{0, "bytes", "8 B"}, {1, "day", "+20 B (partial)"}, {1, "week", "Unavailable"},
	} {
		if got := view.storageFor(tc.id)[tc.key].text(tc.key); got != tc.want {
			t.Errorf("service %d %s = %q, want %q", tc.id, tc.key, got, tc.want)
		}
	}
	if got := newResourceView(resourceSamples{storage: day}, nil, now).storageFor(1)["bytes"].text("bytes"); got != "Unavailable" {
		t.Errorf("stale storage = %s", got)
	}

	start := now.Add(-time.Hour)
	var samples []store.ResourceSnapshot
	for i, state := range []string{"running", "stopped", "running", "running"} {
		values := map[string]float64{"cpu": float64(i)}
		if state == "stopped" {
			values = nil
		}
		samples = append(samples, store.ResourceSnapshot{At: start.Add(time.Duration(i) * time.Minute), Readings: []store.ResourceReading{{Key: "c", State: state, Kind: "container", Values: values}}})
	}
	chart := buildResourceChart(samples, resourceMetric{"cpu", "CPU"}, resourceValue{known: true}, start, now, time.Minute)
	if !strings.HasPrefix(chart.path, "M0.00 98.00") || strings.Count(chart.path, "M") != 2 || strings.Count(chart.path, "L") != 1 {
		t.Errorf("chart must show zero and start a new segment after a stopped container, path=%s", chart.path)
	}
}

func TestResourceOverviewRankingsAndHistory(t *testing.T) {
	srv, st := mcpSettingsServer(t)
	ctx := context.Background()
	var ids []int64
	for _, name := range []string{"library", "shop"} {
		svc := &store.Service{Name: name, Policy: store.PolicyManual, WatchedImage: name}
		if err := st.CreateService(ctx, svc); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, svc.ID)
	}
	now := time.Now().Add(-time.Minute)
	for i, at := range []time.Time{now.Add(-7 * 24 * time.Hour), now.Add(-24 * time.Hour), now} {
		err := st.SaveResourceSnapshot(ctx, "storage", store.ResourceSnapshot{At: at, Readings: []store.ResourceReading{
			{Key: "shared", Name: "Media volume", Kind: "volume", Services: ids, State: "available", Values: map[string]float64{"bytes": float64(100 + i)}},
			{Key: "backups", Name: "Archive files", Kind: "backup", Services: ids[:1], State: "available", Values: map[string]float64{"bytes": float64(1 + i*40)}},
			{Key: "unowned", Name: "Old logs", Kind: "logs", State: "unavailable"},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateDeployment(ctx, &store.Deployment{ServiceID: ids[0], Trigger: store.TriggerManual, Status: store.DeploySuccess}); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, srv)
	for _, tc := range []struct{ query, first, second string }{
		{"bytes", "Media volume", "Archive files"}, {"day", "Archive files", "Media volume"}, {"week", "Archive files", "Media volume"},
	} {
		page := getAuthed(t, srv, cookies, "/resources?sort="+tc.query)
		a, b := strings.Index(page, tc.first), strings.Index(page, tc.second)
		if a < 0 || b < a || !strings.Contains(page, "Shared · library, shop") || !strings.Contains(page, "Unattributed") {
			t.Fatalf("incorrect source ranking or ownership, sort=%s", tc.query)
		}
	}
	page := getAuthed(t, srv, cookies, "/services/library/resources?range=7d")
	for _, want := range []string{"resource-marker", "Deployment #", "Archive files", "Media volume", "+80 B"} {
		if !strings.Contains(page, want) {
			t.Errorf("service history missing %q", want)
		}
	}
}

func TestResourceMemoryLimitsRepresentUnrestrictedContainers(t *testing.T) {
	now := time.Now()
	instances := []store.ResourceReading{
		{Key: "a", Kind: "container", State: "running", Services: []int64{1}, Values: map[string]float64{"limit": 1 << 30}},
		{Key: "b", Kind: "container", State: "running", Services: []int64{1}, Values: map[string]float64{"unlimited": 1}},
	}
	view := newResourceView(resourceSamples{runtime: store.ResourceSnapshot{At: now, Readings: instances}}, nil, now)
	if got := view.runtime.subtotal(1, "limit").text("limit"); got != "Unlimited" {
		t.Errorf("service memory limit = %q", got)
	}
	if got := view.runtime.one("b", "limit").text("limit"); got != "Unlimited" {
		t.Errorf("container memory limit = %q", got)
	}
	chart := buildResourceChart([]store.ResourceSnapshot{{At: now.Add(-2 * time.Minute), Readings: instances[:1]}, {At: now.Add(-time.Minute), Readings: instances}, {At: now, Readings: instances[:1]}}, resourceMetric{"limit", "Memory limit"}, view.runtime.subtotal(1, "limit"), now.Add(-time.Hour), now, time.Minute)
	if strings.Count(chart.path, "M") != 2 || strings.Contains(chart.path, "L") || chart.current != "Unlimited" {
		t.Errorf("unexpected limit chart: %+v", chart)
	}
}

func TestStorageGrowthForNewDockerObjects(t *testing.T) {
	now := time.Now()
	reading := func(kind, name string) store.ResourceReading {
		return store.ResourceReading{Kind: kind, Key: kind + ":" + name, Name: name, State: "available", Values: map[string]float64{"bytes": 80}}
	}
	old := []store.ResourceReading{reading("postgres-volume", "reclassified"), reading("writable", "existing")}
	missing := reading("logs", "unknown")
	missing.State = "unavailable"
	missing.Values = nil
	old = append(old, missing)
	current := []store.ResourceReading{reading("volume", "new volume"), reading("writable", "new layer"), reading("directory", "selected"), reading("logs", "unknown"), reading("logs", "existing"), reading("volume", "reclassified")}
	for _, partial := range []bool{false, true} {
		prior := slices.Clone(old)
		if partial {
			prior = append(prior, store.ResourceReading{Key: "inventory:volumes", Kind: "inventory", State: "unavailable"})
		}
		view := newResourceView(resourceSamples{storage: store.ResourceSnapshot{At: now, Readings: current}, day: store.ResourceSnapshot{At: now.Add(-24 * time.Hour), Readings: prior}}, nil, now)
		for _, row := range view.sourceTable(-1, false).rows {
			want := "Unavailable"
			switch row.name {
			case "new volume", "new layer":
				if !partial {
					want = "+80 B"
				}
			case "reclassified":
				want = "0 B"
			}
			if got := row.values["day"].text("day"); got != want {
				t.Errorf("partial=%t %s growth=%q, want %q", partial, row.name, got, want)
			}
		}
	}
}
