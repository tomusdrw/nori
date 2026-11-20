package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"slices"
	"strings"
)

type ResourceLimits struct {
	UsageBytes      int64
	GrowthBytes     int64
	PostgresDetails bool
	Runtime         map[string]float64
	Directories     []string
}

var resourceRuntimeMetrics = []string{"cpu", "memory", "rx", "tx", "read", "write", "pids"}

func encodeRuntimeLimits(limits map[string]float64) (string, error) {
	enabled := make(map[string]float64)
	for metric, value := range limits {
		if !slices.Contains(resourceRuntimeMetrics, metric) || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
			return "", fmt.Errorf("invalid threshold for runtime metric %q", metric)
		}
		if value > 0 {
			enabled[metric] = value
		}
	}
	raw, err := json.Marshal(enabled)
	return string(raw), err
}

var ErrInvalidResourceDirectories = errors.New("invalid resource directories")

func encodeResourceDirectories(dirs []string) (string, error) {
	if len(dirs) == 0 {
		return "[]", nil
	}
	if len(dirs) > 32 {
		return "", fmt.Errorf("%w: choose at most 32 directories", ErrInvalidResourceDirectories)
	}
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if !path.IsAbs(dir) || path.Clean(dir) != dir || dir == "/" || strings.ContainsRune(dir, 0) {
			return "", fmt.Errorf("%w: %q must be a clean absolute directory other than /", ErrInvalidResourceDirectories, dir)
		}
		tree, _, _ := strings.Cut(dir[1:], "/")
		if tree == "dev" || tree == "sys" || tree == "proc" {
			return "", fmt.Errorf("%w: cannot monitor /%s", ErrInvalidResourceDirectories, tree)
		}
		if seen[dir] {
			return "", fmt.Errorf("%w: %q is listed twice", ErrInvalidResourceDirectories, dir)
		}
		seen[dir] = true
	}
	data, err := json.Marshal(dirs)
	return string(data), err
}

func decodeResourceSettings(limits *ResourceLimits, runtime, dirs string) error {
	if dirs != "[]" {
		if err := json.Unmarshal([]byte(dirs), &limits.Directories); err != nil {
			return err
		}
	}
	if runtime == "{}" {
		return nil
	}
	return json.Unmarshal([]byte(runtime), &limits.Runtime)
}

const resourceLimitsQuery = `SELECT COALESCE(usage_bytes, 0), COALESCE(growth_bytes, 0), COALESCE(postgres_details, 0), COALESCE(runtime, '{}'), COALESCE(directories, '[]')
	FROM service LEFT JOIN service_resource_limits ON service_id=service.id WHERE service.id=?`

func scanResourceLimits(row rowScanner) (ResourceLimits, error) {
	var limits ResourceLimits
	var raw, dirs string
	err := row.Scan(&limits.UsageBytes, &limits.GrowthBytes, &limits.PostgresDetails, &raw, &dirs)
	if errors.Is(err, sql.ErrNoRows) {
		return limits, ErrNotFound
	}
	if err != nil {
		return limits, err
	}
	err = decodeResourceSettings(&limits, raw, dirs)
	return limits, err
}

func (s *Store) ResourceLimits(ctx context.Context, id int64) (ResourceLimits, error) {
	return scanResourceLimits(s.db.QueryRowContext(ctx, resourceLimitsQuery, id))
}

// ActiveResourceLimits reads all services with opt-in collection or positive
// alert thresholds in one query, independent of the number of services.
func (s *Store) ActiveResourceLimits(ctx context.Context) (map[int64]ResourceLimits, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT service_id, usage_bytes, growth_bytes, postgres_details, runtime, directories
		FROM service_resource_limits WHERE usage_bytes>0 OR growth_bytes>0 OR postgres_details OR runtime<>'{}' OR directories<>'[]'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	active := make(map[int64]ResourceLimits)
	for rows.Next() {
		var limits ResourceLimits
		var id int64
		var raw, dirs string
		if err := rows.Scan(&id, &limits.UsageBytes, &limits.GrowthBytes, &limits.PostgresDetails, &raw, &dirs); err != nil {
			return nil, err
		}
		if err := decodeResourceSettings(&limits, raw, dirs); err != nil {
			return nil, err
		}
		active[id] = limits
	}
	return active, rows.Err()
}

// SetResourceLimits rearms each alert when its threshold changes. Settings and
// latches update atomically, including when another administrator saves changes.
func (s *Store) SetResourceLimits(ctx context.Context, id int64, limits ResourceLimits) error {
	_, err := s.UpdateResourceLimits(ctx, id, func(current *ResourceLimits) {
		*current = limits
	})
	return err
}

// UpdateResourceLimits reads and applies an edit in the same write transaction.
func (s *Store) UpdateResourceLimits(ctx context.Context, id int64, apply func(*ResourceLimits)) (ResourceLimits, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ResourceLimits{}, err
	}
	defer tx.Rollback()
	old, err := scanResourceLimits(tx.QueryRowContext(ctx, resourceLimitsQuery, id))
	if err != nil {
		return ResourceLimits{}, err
	}
	limits := old
	limits.Runtime = maps.Clone(old.Runtime)
	limits.Directories = slices.Clone(old.Directories)
	apply(&limits)
	if err := saveResourceLimits(ctx, tx, id, old, limits); err != nil {
		return ResourceLimits{}, err
	}
	limits, err = scanResourceLimits(tx.QueryRowContext(ctx, resourceLimitsQuery, id))
	if err != nil {
		return ResourceLimits{}, err
	}
	return limits, tx.Commit()
}

func saveResourceLimits(ctx context.Context, tx *sql.Tx, id int64, old, limits ResourceLimits) error {
	if limits.UsageBytes < 0 || limits.GrowthBytes < 0 {
		return errors.New("resource thresholds must be non-negative")
	}
	raw, err := encodeRuntimeLimits(limits.Runtime)
	if err != nil {
		return err
	}
	dirs, err := encodeResourceDirectories(limits.Directories)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO service_resource_limits(service_id,usage_bytes,growth_bytes,postgres_details,runtime,directories)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(service_id) DO UPDATE SET usage_bytes=excluded.usage_bytes,growth_bytes=excluded.growth_bytes,
			postgres_details=excluded.postgres_details,runtime=excluded.runtime,directories=excluded.directories`, id, limits.UsageBytes, limits.GrowthBytes, limits.PostgresDetails, raw, dirs)
	if err != nil {
		return err
	}
	var reset []string
	if !sameAlertSettings("usage", old, limits) {
		reset = append(reset, "usage")
	}
	if !sameAlertSettings("growth", old, limits) {
		reset = append(reset, "growth")
	}
	for _, metric := range resourceRuntimeMetrics {
		if old.Runtime[metric] != limits.Runtime[metric] {
			reset = append(reset, "runtime:"+metric)
		}
	}
	for _, kind := range reset {
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_alert WHERE service_id=? AND kind=?`, id, kind); err != nil {
			return err
		}
	}
	return nil
}

func validResourceAlert(kind string) bool {
	return kind == "usage" || kind == "growth" || strings.HasPrefix(kind, "runtime:") && slices.Contains(resourceRuntimeMetrics, strings.TrimPrefix(kind, "runtime:"))
}

// ResourceAlertCoverage reports which sources were missing on delivery. A
// present row with empty coverage is distinct from an inactive alert.
func (s *Store) ResourceAlertCoverage(ctx context.Context, id int64, kind string) (coverage string, active bool, err error) {
	if !validResourceAlert(kind) {
		return "", false, errors.New("unknown resource alert kind")
	}
	err = s.db.QueryRowContext(ctx, `SELECT coverage FROM resource_alert WHERE service_id=? AND kind=?`, id, kind).Scan(&coverage)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return coverage, err == nil, err
}

func sameResourceDirectories(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// Validation already guarantees distinct directories.
	for _, path := range a {
		if !slices.Contains(b, path) {
			return false
		}
	}
	return true
}

func sameAlertSettings(kind string, a, b ResourceLimits) bool {
	switch kind {
	case "usage":
		return a.UsageBytes == b.UsageBytes && sameResourceDirectories(a.Directories, b.Directories)
	case "growth":
		return a.GrowthBytes == b.GrowthBytes && sameResourceDirectories(a.Directories, b.Directories)
	default:
		metric := strings.TrimPrefix(kind, "runtime:")
		return a.Runtime[metric] == b.Runtime[metric]
	}
}

// SetResourceAlertIfCurrent applies the result of an alert evaluation only if
// its threshold and storage coverage still match the settings read before
// notification delivery. An administrator may change either during delivery.
func (s *Store) SetResourceAlertIfCurrent(ctx context.Context, id int64, kind string, active bool, expected ResourceLimits, coverage string) error {
	if !validResourceAlert(kind) {
		return errors.New("unknown resource alert kind")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanResourceLimits(tx.QueryRowContext(ctx, resourceLimitsQuery, id))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !sameAlertSettings(kind, expected, current) {
		return nil
	}
	if active {
		_, err = tx.ExecContext(ctx, `INSERT INTO resource_alert(service_id,kind,coverage) VALUES (?,?,?)
			ON CONFLICT(service_id,kind) DO UPDATE SET coverage=excluded.coverage`, id, kind, coverage)
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM resource_alert WHERE service_id=? AND kind=?`, id, kind)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
