package resources

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"time"

	"nori/internal/store"
)

type usage struct {
	bytes               float64
	measured            bool
	missing             map[string]bool
	incompleteInventory bool
}

func serviceUsage(snap store.ResourceSnapshot) map[int64]*usage {
	totals := make(map[int64]*usage)
	incompleteInventory := false
	for _, r := range snap.Readings {
		if r.Kind == "inventory" && len(r.Services) == 0 {
			incompleteInventory = true
		}
		if r.Kind == "postgres-database" || r.Kind == "postgres-table" {
			continue
		}
		for _, id := range r.Services {
			total := totals[id]
			if total == nil {
				total = &usage{missing: make(map[string]bool)}
				totals[id] = total
			}
			if r.Kind == "inventory" {
				total.incompleteInventory = true
			}
			if v, ok := r.Values["bytes"]; ok {
				total.bytes += v
				total.measured = true
			} else {
				total.missing[r.Key] = true
			}
		}
	}
	if incompleteInventory {
		for _, total := range totals {
			total.incompleteInventory = true
		}
	}
	return totals
}

func (m *Monitor) checkAlerts(ctx context.Context, snap store.ResourceSnapshot) error {
	configs, err := m.store.ActiveResourceLimits(ctx)
	if err != nil {
		return err
	}
	now := serviceUsage(snap)
	var half, day map[int64]*usage
	var elapsed1, elapsed2 time.Duration
	var baselinesLoaded bool
	var errs error
	for id, limits := range configs {
		if limits.UsageBytes == 0 && limits.GrowthBytes == 0 {
			continue
		}
		svc, err := m.store.GetService(ctx, id)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		total := now[svc.ID]
		if total == nil || !total.measured {
			continue
		}
		if limits.UsageBytes > 0 {
			exceeded := total.bytes >= float64(limits.UsageBytes)
			// A measured subtotal can prove a breach even with incomplete inventory.
			if exceeded || !total.incompleteInventory {
				msg := fmt.Sprintf("Nori: %s uses at least %s of storage (limit %s).", svc.Name, FormatAmount("bytes", total.bytes), FormatAmount("bytes", float64(limits.UsageBytes)))
				errs = errors.Join(errs, m.transition(ctx, svc, limits, "usage", exceeded, msg, missingSignature(total.missing)))
			}
		}
		if limits.GrowthBytes == 0 {
			continue
		}
		if !baselinesLoaded {
			h, err := m.baseline(ctx, snap.At.Add(-12*time.Hour))
			if err != nil {
				return errors.Join(errs, err)
			}
			d, err := m.baseline(ctx, snap.At.Add(-24*time.Hour))
			if err != nil {
				return errors.Join(errs, err)
			}
			half, day = serviceUsage(h), serviceUsage(d)
			elapsed1, elapsed2 = h.At.Sub(d.At), snap.At.Sub(h.At)
			baselinesLoaded = true
		}
		rate, ok := sustainedRate(day[svc.ID], half[svc.ID], total, elapsed1, elapsed2)
		if !ok {
			continue
		}
		msg := fmt.Sprintf("Nori: %s storage grew at least %s/day in both half-day windows (limit %s/day).", svc.Name, FormatAmount("bytes", rate), FormatAmount("bytes", float64(limits.GrowthBytes)))
		errs = errors.Join(errs, m.transition(ctx, svc, limits, "growth", rate >= float64(limits.GrowthBytes), msg, missingSignature(total.missing)))
	}
	return errs
}

func (m *Monitor) baseline(ctx context.Context, target time.Time) (store.ResourceSnapshot, error) {
	snap, err := m.store.LatestResourceSnapshot(ctx, "storage", target)
	// Downsampled data may be up to one hour before the requested point.
	if target.Sub(snap.At) > time.Hour+StorageInterval {
		return store.ResourceSnapshot{}, err
	}
	return snap, err
}

// Compare only like-for-like coverage. A source becoming measurable can
// otherwise look like a sudden increase in storage.
func sustainedRate(first, middle, last *usage, a, b time.Duration) (float64, bool) {
	if first == nil || middle == nil || last == nil || !first.measured || !middle.measured || !last.measured ||
		first.incompleteInventory || middle.incompleteInventory || last.incompleteInventory ||
		a <= 0 || b <= 0 || !maps.Equal(first.missing, middle.missing) || !maps.Equal(middle.missing, last.missing) {
		return 0, false
	}
	return min((middle.bytes-first.bytes)*24/a.Hours(), (last.bytes-middle.bytes)*24/b.Hours()), true
}

// Keep the coverage at the time of a breach. Sources that a driver never
// exposes should not prevent a measured subtotal from crossing back below
// its threshold. Losing visibility of another source cannot prove recovery.
func missingSignature(missing map[string]bool) string {
	if len(missing) == 0 {
		return ""
	}
	keys, _ := json.Marshal(slices.Sorted(maps.Keys(missing)))
	return fmt.Sprintf("%x", sha256.Sum256(keys))
}

func (m *Monitor) transition(ctx context.Context, svc *store.Service, limits store.ResourceLimits, kind string, exceeded bool, message, coverage string) error {
	previous, active, err := m.store.ResourceAlertCoverage(ctx, svc.ID, kind)
	if err != nil || active == exceeded {
		return err
	}
	if !exceeded && coverage != "" && coverage != previous {
		return nil
	}
	if exceeded {
		if err := m.notify(ctx, message, "/services/"+url.PathEscape(svc.Name)+"/resources"); err != nil {
			return err
		}
	}
	return m.store.SetResourceAlertIfCurrent(ctx, svc.ID, kind, exceeded, limits, coverage)
}

func (m *Monitor) checkRuntimeAlerts(ctx context.Context, snap store.ResourceSnapshot) error {
	configs, err := m.store.ActiveResourceLimits(ctx)
	if err != nil {
		return err
	}
	containers := make(map[int64][]store.ResourceReading)
	for _, r := range snap.Readings {
		for _, id := range r.Services {
			if len(configs[id].Runtime) > 0 {
				containers[id] = append(containers[id], r)
			}
		}
	}
	var errs error
	for id, readings := range containers {
		svc, err := m.store.GetService(ctx, id)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		for metric, limit := range configs[id].Runtime {
			var sum float64
			missing := make(map[string]bool)
			known := false
			for _, r := range readings {
				if r.State == "stopped" {
					known = true
					continue
				}
				v, ok := r.Values[metric]
				sum += v
				if ok {
					known = true
				} else {
					missing[r.Key] = true
				}
			}
			exceeded := sum >= limit
			if !known {
				continue
			}
			msg := fmt.Sprintf("Nori: %s %s usage is %s (limit %s).", svc.Name, metric, FormatAmount(metric, sum), FormatAmount(metric, limit))
			errs = errors.Join(errs, m.transition(ctx, svc, configs[id], "runtime:"+metric, exceeded, msg, missingSignature(missing)))
		}
	}
	return errs
}
