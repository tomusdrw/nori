package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nori/internal/resources"
	"nori/internal/store"
)

type resourceChart struct {
	label, current, peak, path string
}

type resourceMarker struct {
	x, label, url string
}

type resourceRange struct {
	key, startLabel, endLabel string
	markers                   []resourceMarker
}

type serviceResourceData struct {
	svc                          *store.Service
	view                         resourceView
	limits                       store.ResourceLimits
	containers, sources, logical resourceTable
	charts                       []resourceChart
	period                       resourceRange
	saved                        bool
}

func resourceWindow(value string) (string, time.Duration) {
	switch value {
	case "1h":
		return "1h", time.Hour
	case "30d":
		return "30d", 30 * 24 * time.Hour
	case "7d":
		return "7d", 7 * 24 * time.Hour
	default:
		return "24h", 24 * time.Hour
	}
}

func chartX(at, start, end time.Time) float64 {
	return 1000 * float64(at.Sub(start)) / float64(end.Sub(start))
}

func chartMarkers(deployments []*store.Deployment, start, end time.Time) []resourceMarker {
	var markers []resourceMarker
	for _, d := range deployments {
		if d.StartedAt.Before(start) || d.StartedAt.After(end) {
			continue
		}
		id := strconv.FormatInt(d.ID, 10)
		markers = append(markers, resourceMarker{
			x:     fmt.Sprintf("%.2f", chartX(d.StartedAt, start, end)),
			label: "Deployment #" + id + " · " + d.StartedAt.UTC().Format("02 Jan 15:04 MST") + " · " + d.Status,
			url:   "/deployments/" + id,
		})
	}
	return markers
}

func buildResourceChart(history []store.ResourceSnapshot, metric resourceMetric, current resourceValue, start, end time.Time, cadence time.Duration) resourceChart {
	chart := resourceChart{label: metric.label, current: current.text(metric.key)}
	type point struct {
		at    time.Time
		value resourceValue
	}
	points := make([]point, 0, len(history))
	var peak float64
	for _, sample := range history {
		v := measuredResource(sample.Readings, metric.key)
		points = append(points, point{sample.At, v})
		if v.known && v.number > peak {
			peak = v.number
		}
	}
	chart.peak = resources.FormatAmount(metric.key, peak)

	var path strings.Builder
	var previous point
	for _, p := range points {
		if p.value.known {
			y := 98.0
			if peak != 0 {
				y -= 96 * p.value.number / peak
			}
			// After raw data expires, the store retains one actual sample per hour.
			gap := 2 * cadence
			if previous.at.Before(end.Add(-24 * time.Hour)) {
				gap = max(gap, 75*time.Minute)
			}
			move := !previous.value.known || p.at.Sub(previous.at) > gap || !sameCoverage(previous.value, p.value)
			if move {
				// A short segment with a round cap makes an isolated sample visible.
				fmt.Fprintf(&path, "M%.2f %.2fh0.01", chartX(p.at, start, end), y)
			} else {
				fmt.Fprintf(&path, "L%.2f %.2f", chartX(p.at, start, end), y)
			}
		}
		previous = p
	}
	chart.path = path.String()
	return chart
}

func (s *Server) handleServiceResources(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	ctx := r.Context()
	now := time.Now()
	rangeKey, window := resourceWindow(r.URL.Query().Get("range"))
	start := now.Add(-window)
	view, err := s.loadResourceView(ctx, now)
	if err != nil {
		http.Error(w, "Could not load resource inventory", http.StatusInternalServerError)
		return
	}
	limits, err := s.store.ResourceLimits(ctx, svc.ID)
	if err != nil {
		http.Error(w, "Could not load resource settings", http.StatusInternalServerError)
		return
	}
	deployments, err := s.store.ListResourceDeployments(ctx, svc.ID, start, now)
	if err != nil {
		http.Error(w, "Could not load deployment history", http.StatusInternalServerError)
		return
	}
	data := serviceResourceData{svc: svc, view: view, limits: limits, containers: view.containers(svc.ID),
		sources: view.sourceTable(svc.ID, false), logical: view.sourceTable(svc.ID, true), saved: r.URL.Query().Get("saved") == "1",
		period: resourceRange{key: rangeKey, startLabel: start.UTC().Format("02 Jan 15:04 MST"), endLabel: now.UTC().Format("02 Jan 15:04 MST"), markers: chartMarkers(deployments, start, now)},
	}
	for _, q := range []struct {
		stream    string
		metrics   []resourceMetric
		inventory resourceInventory
		cadence   time.Duration
	}{
		{"runtime", runtimeMetrics, view.runtime, resources.RuntimeInterval},
		{"storage", []resourceMetric{{"bytes", "Storage used"}}, view.storage, resources.StorageInterval},
	} {
		history, err := s.store.ResourceChartHistory(ctx, q.stream, start, now, int(window/(q.cadence/2))+1, svc.ID)
		if err != nil {
			http.Error(w, "Could not load resource history", http.StatusInternalServerError)
			return
		}
		for _, metric := range q.metrics {
			current := q.inventory.subtotal(svc.ID, metric.key)
			data.charts = append(data.charts, buildResourceChart(history, metric, current, start, now, q.cadence))
		}
	}
	_ = serviceResourcesPage(data, s.csrf(r)).Render(ctx, w)
}
