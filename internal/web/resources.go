package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"nori/internal/resources"
	"nori/internal/store"
)

type resourceMetric struct{ key, label string }

var runtimeMetrics = []resourceMetric{
	{"cpu", "CPU"}, {"memory", "Memory used"}, {"limit", "Memory limit"},
	{"rx", "Network in"}, {"tx", "Network out"}, {"read", "Disk read"}, {"write", "Disk write"}, {"pids", "Processes / threads"},
}

var storageMetrics = []resourceMetric{{"bytes", "Current use"}, {"day", "24h growth"}, {"week", "7d growth"}}

type resourceValue struct {
	incompleteInventory bool
	number              float64
	known               bool
	unlimited           bool
	missing             []string
}

func (v resourceValue) text(key string) string {
	if v.unlimited {
		return "Unlimited"
	}
	if !v.known {
		return "Unavailable"
	}
	label := resources.FormatAmount(key, v.number)
	if len(v.missing) != 0 {
		return label + " (partial)"
	}
	return label
}

func sameCoverage(a, b resourceValue) bool {
	return !a.incompleteInventory && !b.incompleteInventory && slices.Equal(a.missing, b.missing)
}

func resourceChange(current, baseline resourceValue) resourceValue {
	if !current.known || !baseline.known || !sameCoverage(current, baseline) {
		return resourceValue{}
	}
	current.number -= baseline.number
	return current
}

func logicalResource(kind string) bool {
	return kind == "postgres-database" || kind == "postgres-table"
}

func readingValue(r store.ResourceReading, key string) resourceValue {
	if r.State == "unavailable" || r.State == "stopped" {
		return resourceValue{}
	}
	if key == "limit" && r.Values["unlimited"] == 1 {
		return resourceValue{unlimited: true}
	}
	n, ok := r.Values[key]
	return resourceValue{number: n, known: ok}
}

// The instance total traverses the inventory once, including sources with no owner.
// A service subtotal includes the full size of each attached shared source.
func measuredResource(readings []store.ResourceReading, key string) resourceValue {
	var result resourceValue
	for _, r := range readings {
		if logicalResource(r.Kind) || r.State == "stopped" {
			continue
		}
		if r.Kind == "inventory" {
			result.incompleteInventory = true
		}
		v := readingValue(r, key)
		if v.unlimited {
			return v
		}
		if !v.known {
			result.missing = append(result.missing, r.Key)
			continue
		}
		result.number += v.number
		result.known = true
	}
	sort.Strings(result.missing)
	return result
}

func resourceFresh(s store.ResourceSnapshot, target time.Time, tolerance time.Duration) bool {
	return !s.At.IsZero() && !s.At.After(target) && target.Sub(s.At) <= tolerance
}

type resourceSamples struct {
	runtime, storage, day, week store.ResourceSnapshot
}

func (s *Server) resourceSamples(ctx context.Context, now time.Time) (resourceSamples, error) {
	var samples resourceSamples
	for _, q := range []struct {
		stream string
		at     time.Time
		dest   *store.ResourceSnapshot
	}{
		{"runtime", now, &samples.runtime}, {"storage", now, &samples.storage},
		{"storage", now.Add(-24 * time.Hour), &samples.day}, {"storage", now.Add(-7 * 24 * time.Hour), &samples.week},
	} {
		result, err := s.store.LatestResourceSnapshot(ctx, q.stream, q.at)
		if err != nil {
			return samples, err
		}
		*q.dest = result
	}
	return samples, nil
}

type resourceInventory struct {
	incomplete bool
	at         time.Time
	stale      bool
	all        []store.ResourceReading
	byService  map[int64][]store.ResourceReading
	byKey      map[string]store.ResourceReading
	unknown    []store.ResourceReading
}

func indexResources(s store.ResourceSnapshot, at time.Time, tolerance time.Duration) resourceInventory {
	idx := resourceInventory{at: s.At, stale: !resourceFresh(s, at, tolerance), all: s.Readings, byService: make(map[int64][]store.ResourceReading), byKey: make(map[string]store.ResourceReading)}
	for _, r := range s.Readings {
		idx.byKey[r.Key] = r
		if r.Kind == "inventory" {
			idx.incomplete = true
		}
		if len(r.Services) == 0 {
			idx.byService[0] = append(idx.byService[0], r)
			if r.Kind == "inventory" {
				idx.unknown = append(idx.unknown, r)
			}
		}
		for _, id := range r.Services {
			idx.byService[id] = append(idx.byService[id], r)
		}
	}
	return idx
}

func (idx resourceInventory) subtotal(id int64, metric string) resourceValue {
	if idx.stale {
		return resourceValue{}
	}
	rows := idx.byService[id]
	if id < 0 {
		rows = idx.all
	}
	value := measuredResource(rows, metric)
	if id > 0 {
		for _, r := range idx.unknown {
			value.incompleteInventory = true
			value.missing = append(value.missing, r.Key)
		}
		sort.Strings(value.missing)
	}
	return value
}

func (idx resourceInventory) one(key, metric string) resourceValue {
	if idx.stale {
		return resourceValue{}
	}
	return readingValue(idx.byKey[key], metric)
}

// A complete Docker inventory establishes zero prior usage for a new object.
// Optional directory measurements and unsupported metrics carry no such evidence.
func (idx resourceInventory) sourceBaseline(r store.ResourceReading) resourceValue {
	if idx.stale {
		return resourceValue{}
	}
	if old, ok := idx.byKey[r.Key]; ok {
		return readingValue(old, "bytes")
	}
	id, ok := strings.CutPrefix(r.Key, r.Kind+":")
	if !ok {
		return resourceValue{}
	}
	switch r.Kind {
	case "volume", "postgres-volume":
		for _, kind := range []string{"volume", "postgres-volume"} {
			if old, ok := idx.byKey[kind+":"+id]; ok {
				return readingValue(old, "bytes")
			}
			if _, ok := idx.byKey[kind+":missing:"+id]; ok {
				return resourceValue{}
			}
		}
	case "logs", "writable":
		// A known container without this measurement has an unknown baseline.
		for _, kind := range []string{"logs", "writable"} {
			if _, ok := idx.byKey[kind+":"+id]; ok {
				return resourceValue{}
			}
		}
	default:
		return resourceValue{}
	}
	return resourceValue{known: !idx.incomplete}
}

func (idx resourceInventory) timestamp() string {
	if idx.at.IsZero() {
		return "No samples yet"
	}
	s := idx.at.UTC().Format("02 Jan 15:04 MST")
	if idx.stale {
		return s + " · stale"
	}
	return s
}

type resourceRow struct {
	name, url, detail string
	values            map[string]resourceValue
}

type resourceTable struct {
	title, first, context string
	columns               []resourceMetric
	rows                  []resourceRow
	sortKey, order        string
	sortable              bool
}

func (t *resourceTable) rank(key, order string) {
	t.sortable = true
	t.sortKey = t.columns[0].key
	for _, col := range t.columns {
		if col.key == key {
			t.sortKey = key
		}
	}
	t.order = "desc"
	if key == t.sortKey && order == "asc" {
		t.order = "asc"
	}
	sort.SliceStable(t.rows, func(i, j int) bool {
		a, b := t.rows[i].values[t.sortKey], t.rows[j].values[t.sortKey]
		aAvailable, bAvailable := a.known || a.unlimited, b.known || b.unlimited
		if aAvailable != bAvailable {
			return aAvailable
		}
		if a.unlimited != b.unlimited {
			if t.order == "asc" {
				return b.unlimited
			}
			return a.unlimited
		}
		if a.number == b.number {
			return t.rows[i].name < t.rows[j].name
		}
		if t.order == "asc" {
			return a.number < b.number
		}
		return a.number > b.number
	})
}

func (t resourceTable) sortURL(key string) string {
	order := "desc"
	if key == t.sortKey && t.order == "desc" {
		order = "asc"
	}
	return "?sort=" + key + "&order=" + order
}

func (t resourceTable) sortDirection(key string) string {
	if key != t.sortKey {
		return "none"
	}
	if t.order == "asc" {
		return "ascending"
	}
	return "descending"
}

func serviceResourcesURL(name string) string {
	return "/services/" + url.PathEscape(name) + "/resources"
}

func sourceKind(kind string) string {
	switch kind {
	case "volume":
		return "Docker volume"
	case "postgres-volume":
		return "PostgreSQL volume"
	case "directory":
		return "Host directory"
	case "writable":
		return "Writable layer"
	case "logs":
		return "Container logs"
	case "backup":
		return "Local backup archives"
	case "postgres-database":
		return "PostgreSQL database"
	case "postgres-table":
		return "PostgreSQL table"
	case "inventory":
		return "Inventory"
	default:
		return kind
	}
}

func resourceOwners(ids []int64, services map[int64]string) string {
	if len(ids) == 0 {
		return "Unattributed"
	}
	var names []string
	for _, id := range ids {
		name := services[id]
		if name == "" {
			name = fmt.Sprintf("Service #%d", id)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	label := strings.Join(names, ", ")
	if len(names) > 1 {
		return "Shared · " + label
	}
	return label
}

type resourceView struct {
	runtime, storage resourceInventory
	day, week        resourceInventory
	services         map[int64]string
}

func newResourceView(samples resourceSamples, services []*store.Service, now time.Time) resourceView {
	view := resourceView{
		runtime: indexResources(samples.runtime, now, 2*resources.RuntimeInterval), storage: indexResources(samples.storage, now, 2*resources.StorageInterval),
		day:      indexResources(samples.day, now.Add(-24*time.Hour), time.Hour+resources.StorageInterval),
		week:     indexResources(samples.week, now.Add(-7*24*time.Hour), time.Hour+resources.StorageInterval),
		services: make(map[int64]string),
	}
	for _, svc := range services {
		view.services[svc.ID] = svc.Name
	}
	return view
}

func sizeAndGrowth(now, day, week resourceValue) map[string]resourceValue {
	return map[string]resourceValue{"bytes": now, "day": resourceChange(now, day), "week": resourceChange(now, week)}
}

func (v resourceView) storageFor(id int64) map[string]resourceValue {
	return sizeAndGrowth(v.storage.subtotal(id, "bytes"), v.day.subtotal(id, "bytes"), v.week.subtotal(id, "bytes"))
}

func (v resourceView) sourceTable(serviceID int64, logical bool) resourceTable {
	table := resourceTable{title: "Storage sources", first: "Source", context: "Type · ownership", columns: storageMetrics}
	metric := "bytes"
	if logical {
		table.title = "PostgreSQL logical sizes"
		table.columns = []resourceMetric{{"logical", "Logical size"}}
		metric = "logical"
	}
	for _, r := range v.storage.all {
		if logical != logicalResource(r.Kind) || (serviceID > 0 && !slices.Contains(r.Services, serviceID) && !(r.Kind == "inventory" && len(r.Services) == 0)) {
			continue
		}
		current := v.storage.one(r.Key, metric)
		values := map[string]resourceValue{"logical": current}
		if !logical {
			values = sizeAndGrowth(current, v.day.sourceBaseline(r), v.week.sourceBaseline(r))
		}
		table.rows = append(table.rows, resourceRow{name: r.Name, detail: sourceKind(r.Kind) + " · " + resourceOwners(r.Services, v.services), values: values})
	}
	return table
}

func (v resourceView) runtimeFor(id int64) map[string]resourceValue {
	values := make(map[string]resourceValue)
	for _, metric := range runtimeMetrics {
		values[metric.key] = v.runtime.subtotal(id, metric.key)
	}
	return values
}

func (v resourceView) containers(id int64) resourceTable {
	table := resourceTable{title: "Containers", first: "Container", context: "State", columns: runtimeMetrics}
	for _, r := range v.runtime.byService[id] {
		values := make(map[string]resourceValue)
		for _, metric := range runtimeMetrics {
			values[metric.key] = v.runtime.one(r.Key, metric.key)
		}
		state := r.State
		if v.runtime.stale {
			state += " (stale)"
		}
		table.rows = append(table.rows, resourceRow{name: r.Name, detail: state, values: values})
	}
	return table
}

func (s *Server) loadResourceView(ctx context.Context, now time.Time) (resourceView, error) {
	samples, err := s.resourceSamples(ctx, now)
	if err != nil {
		return resourceView{}, err
	}
	services, err := s.store.ListServices(ctx)
	return newResourceView(samples, services, now), err
}

type resourcesOverview struct {
	view                                     resourceView
	services, sources, runtime, unattributed resourceTable
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	view, err := s.loadResourceView(r.Context(), time.Now())
	if err != nil {
		http.Error(w, "Could not load resource inventory", http.StatusInternalServerError)
		return
	}
	data := resourcesOverview{view: view, sources: view.sourceTable(-1, false),
		services: resourceTable{title: "Storage by service", first: "Service", columns: storageMetrics},
		runtime:  resourceTable{title: "Runtime by service", first: "Service", columns: runtimeMetrics},
	}
	for id, name := range view.services {
		data.services.rows = append(data.services.rows, resourceRow{name: name, url: serviceResourcesURL(name), values: view.storageFor(id)})
		data.runtime.rows = append(data.runtime.rows, resourceRow{name: name, url: serviceResourcesURL(name), values: view.runtimeFor(id)})
	}
	if len(view.storage.byService[0]) > 0 {
		data.services.rows = append(data.services.rows, resourceRow{name: "Unattributed", values: view.storageFor(0)})
	}
	if len(view.runtime.byService[0]) > 0 {
		data.runtime.rows = append(data.runtime.rows, resourceRow{name: "Unattributed", values: view.runtimeFor(0)})
		data.unattributed = view.containers(0)
		data.unattributed.title = "Unattributed containers"
		data.unattributed.rank(r.URL.Query().Get("sort"), r.URL.Query().Get("order"))
	}
	for _, table := range []*resourceTable{&data.services, &data.sources, &data.runtime} {
		table.rank(r.URL.Query().Get("sort"), r.URL.Query().Get("order"))
	}
	_ = resourcesPage(data, s.csrf(r)).Render(r.Context(), w)
}
