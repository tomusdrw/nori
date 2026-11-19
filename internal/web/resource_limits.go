package web

import (
	"errors"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"nori/internal/store"
)

var thresholdSize = regexp.MustCompile(`(?i)^([0-9]{1,20}(?:\.[0-9]{1,20})?)\s*(B|KiB|MiB|GiB|TiB|PiB|EiB)?$`)

var thresholdUnits = []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// Store integer bytes exactly, including for fractional binary units and
// thresholds larger than the largest exact integer representable as float64.
func parseByteThreshold(raw string) (int64, bool) {
	match := thresholdSize.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return 0, false
	}
	n, ok := new(big.Rat).SetString(match[1])
	if !ok {
		return 0, false
	}
	for power, unit := range thresholdUnits {
		if strings.EqualFold(unit, match[2]) {
			n.Mul(n, new(big.Rat).SetInt64(1<<(10*power)))
			break
		}
	}
	if !n.IsInt() || !n.Num().IsInt64() {
		return 0, false
	}
	return n.Num().Int64(), true
}

func displayByteThreshold(n int64) string {
	power := 0
	for n > 0 && power < len(thresholdUnits)-1 && n%1024 == 0 {
		n /= 1024
		power++
	}
	return strconv.FormatInt(n, 10) + " " + thresholdUnits[power]
}

func runtimeUnit(key string) string {
	switch key {
	case "cpu":
		return "%"
	case "memory":
		return "bytes"
	case "pids":
		return "count"
	default:
		return "bytes/s"
	}
}

func (s *Server) handleResourceLimits(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	limits := store.ResourceLimits{PostgresDetails: svc.DeploymentMode == store.DeploymentModePostgres && r.FormValue("postgres_details") == "1"}
	for _, field := range []struct {
		name string
		dest *int64
	}{{"usage_bytes", &limits.UsageBytes}, {"growth_bytes", &limits.GrowthBytes}} {
		n, ok := parseByteThreshold(r.FormValue(field.name))
		if !ok {
			http.Error(w, "Enter a non-negative size such as 512 MiB, 1.5 GiB, or 0.", http.StatusBadRequest)
			return
		}
		*field.dest = n
	}
	limits.Runtime = make(map[string]float64)
	for _, metric := range runtimeMetrics {
		if metric.key == "limit" {
			continue
		}
		raw := strings.TrimSpace(r.FormValue("threshold_" + metric.key))
		if raw == "" {
			continue
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			http.Error(w, "Enter a finite, non-negative threshold for "+metric.label+".", http.StatusBadRequest)
			return
		}
		if n > 0 {
			limits.Runtime[metric.key] = n
		}
	}
	for _, line := range strings.Split(r.FormValue("directories"), "\n") {
		if path := strings.TrimSpace(line); path != "" {
			limits.Directories = append(limits.Directories, path)
		}
	}
	if err = s.store.SetResourceLimits(r.Context(), svc.ID, limits); err != nil {
		if errors.Is(err, store.ErrInvalidResourceDirectories) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "Could not save resource settings", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, serviceResourcesURL(svc.Name)+"?saved=1#thresholds", http.StatusSeeOther)
}
