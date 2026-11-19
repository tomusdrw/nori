package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"
)

const resourceSchema = `
CREATE INDEX IF NOT EXISTS deployment_service_started ON deployment(service_id, started_at);
CREATE TABLE IF NOT EXISTS resource_sample (
	stream TEXT NOT NULL,
	at INTEGER NOT NULL,
	hour INTEGER NOT NULL,
	hourly INTEGER NOT NULL,
	readings BLOB NOT NULL,
	PRIMARY KEY(stream, at)
);
CREATE UNIQUE INDEX IF NOT EXISTS resource_hourly ON resource_sample(stream, hour) WHERE hourly=1;
CREATE INDEX IF NOT EXISTS resource_detail ON resource_sample(stream, at) WHERE hourly=0;
CREATE TABLE IF NOT EXISTS service_resource_limits (
	service_id INTEGER PRIMARY KEY REFERENCES service(id) ON DELETE CASCADE,
	usage_bytes INTEGER NOT NULL DEFAULT 0 CHECK(usage_bytes >= 0),
	growth_bytes INTEGER NOT NULL DEFAULT 0 CHECK(growth_bytes >= 0),
	postgres_details INTEGER NOT NULL DEFAULT 0,
	runtime TEXT NOT NULL DEFAULT '{}',
	directories TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS resource_alert (
	service_id INTEGER NOT NULL REFERENCES service(id) ON DELETE CASCADE,
	kind TEXT NOT NULL,
	coverage TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(service_id, kind)
);
`

// ResourceReading records one source. An absent metric is unknown. Services
// lists every owner at the time of collection, including for shared volumes.
type ResourceReading struct {
	Key      string             `json:"key"`
	Name     string             `json:"name,omitempty"`
	Kind     string             `json:"kind"`
	State    string             `json:"state,omitempty"`
	Services []int64            `json:"services,omitempty"`
	Values   map[string]float64 `json:"values,omitempty"`
}

type ResourceSnapshot struct {
	At       time.Time
	Readings []ResourceReading
}

const (
	resourceDetailRetention = 24 * time.Hour
	resourceHourlyRetention = 30 * 24 * time.Hour
)

// SaveResourceSnapshot marks the last observation in each hour for longer
// retention. Both expiry queries seek directly into an index. Using the newest
// saved timestamp as the clock also handles retries and out-of-order samples.
func (s *Store) SaveResourceSnapshot(ctx context.Context, stream string, snap ResourceSnapshot) error {
	if stream == "" || snap.At.IsZero() {
		return errors.New("resource snapshot requires a stream and time")
	}
	data, err := json.Marshal(snap.Readings)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := snap.At.UnixNano()
	hour := snap.At.Truncate(time.Hour).UnixNano()
	if _, err := tx.ExecContext(ctx, `UPDATE resource_sample SET hourly=0
		WHERE stream=? AND hour=? AND hourly=1 AND at<?`, stream, hour, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO resource_sample(stream, at, hour, hourly, readings)
		VALUES (?, ?, ?, NOT EXISTS (
			SELECT 1 FROM resource_sample WHERE stream=? AND hour=? AND hourly=1 AND at>?
		), ?) ON CONFLICT(stream,at) DO UPDATE SET readings=excluded.readings`, stream, at, hour, stream, hour, at, data); err != nil {
		return err
	}
	var newest int64
	if err := tx.QueryRowContext(ctx, `SELECT at FROM resource_sample WHERE stream=? ORDER BY at DESC LIMIT 1`, stream).Scan(&newest); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM resource_sample WHERE stream=? AND hourly=0 AND at<?`, stream, newest-resourceDetailRetention.Nanoseconds()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM resource_sample WHERE stream=? AND at<?`, stream, newest-resourceHourlyRetention.Nanoseconds()); err != nil {
		return err
	}
	return tx.Commit()
}

func scanResourceSnapshot(row rowScanner) (ResourceSnapshot, error) {
	var at int64
	var data []byte
	if err := row.Scan(&at, &data); err != nil {
		return ResourceSnapshot{}, err
	}
	snap := ResourceSnapshot{At: time.Unix(0, at).UTC()}
	err := json.Unmarshal(data, &snap.Readings)
	return snap, err
}

// LatestResourceSnapshot returns the most recent sample at or before before.
// A zero boundary selects the latest sample. A zero snapshot indicates no data.
func (s *Store) LatestResourceSnapshot(ctx context.Context, stream string, before time.Time) (ResourceSnapshot, error) {
	end := int64(math.MaxInt64)
	if !before.IsZero() {
		end = before.UnixNano()
	}
	row := s.db.QueryRowContext(ctx, `SELECT at, readings FROM resource_sample
		WHERE stream=? AND at<=? ORDER BY at DESC LIMIT 1`, stream, end)
	snap, err := scanResourceSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ResourceSnapshot{}, nil
	}
	return snap, err
}

func (s *Store) resourceSnapshots(ctx context.Context, query string, args ...any) ([]ResourceSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []ResourceSnapshot
	for rows.Next() {
		snap, err := scanResourceSnapshot(rows)
		if err != nil {
			return nil, err
		}
		history = append(history, snap)
	}
	return history, rows.Err()
}

// ResourceChartHistory takes the last genuine observation per bucket. A
// service-specific chart keeps incomplete inventory markers and empty samples
// so that missing intervals can be shown as gaps.
func (s *Store) ResourceChartHistory(ctx context.Context, stream string, from, to time.Time, buckets int, serviceID int64) ([]ResourceSnapshot, error) {
	if buckets <= 0 || from.IsZero() || to.IsZero() || to.Before(from) {
		return nil, errors.New("resource chart requires an ordered time range and positive bucket count")
	}
	// Integer division plus one covers both inclusive endpoints in at most
	// buckets groups, including when from and to are the same instant.
	width := to.Sub(from).Nanoseconds()/int64(buckets) + 1
	const chart = `WITH selected AS (
		SELECT MAX(at) AS at FROM resource_sample
		WHERE stream=? AND at BETWEEN ? AND ?
		GROUP BY (at-?)/?
	) SELECT sample.at, `
	readings := "sample.readings"
	args := []any{stream, from.UnixNano(), to.UnixNano(), from.UnixNano(), width}
	if serviceID > 0 {
		readings = `(SELECT json_group_array(json(r.value)) FROM json_each(CAST(sample.readings AS TEXT)) AS r
			WHERE EXISTS (SELECT 1 FROM json_each(r.value, '$.services') WHERE value=?)
				OR (json_extract(r.value, '$.kind')='inventory' AND COALESCE(json_array_length(r.value, '$.services'), 0)=0))`
		args = append(args, serviceID)
	}
	// CROSS JOIN makes SQLite drive the lookup from the small bucket result:
	// each selected timestamp fetches exactly one payload via (stream, at).
	const join = ` FROM selected CROSS JOIN resource_sample AS sample
		WHERE sample.stream=? AND sample.at=selected.at ORDER BY sample.at`
	return s.resourceSnapshots(ctx, chart+readings+join, append(args, stream)...)
}

// ListResourceDeployments reads log-free summaries for the entire chart window.
func (s *Store) ListResourceDeployments(ctx context.Context, serviceID int64, from, to time.Time) ([]*Deployment, error) {
	// Deployment timestamps are stored in whole seconds.
	start := from.Unix()
	if from.Nanosecond() != 0 {
		start++
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,service_id,trigger,target_digest,status,started_at,finished_at,''
		FROM deployment WHERE service_id=? AND started_at BETWEEN ? AND ? ORDER BY started_at,id`, serviceID, start, to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deployments []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}
