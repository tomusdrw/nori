package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

// ResourceClient is the read-only inventory and measurement surface used by the sampler.
type ResourceClient interface {
	ResourceHostName(context.Context) (string, error)
	ResourceContainers(context.Context) ([]container.Summary, error)
	ResourceVolumes(context.Context) ([]*volume.Volume, error)
	ResourceStorage(context.Context, string) (ContainerStorage, error)
	ResourceStats(context.Context, string) (RuntimeStats, error)
	PostgresSizes(ctx context.Context, id, user, database string) ([]PostgresSize, error)
}

type ContainerStorage struct {
	Writable           *int64
	LogPath, LogDriver string
}

func (r *realClient) ResourceHostName(ctx context.Context) (string, error) {
	info, err := r.cli.Info(ctx)
	return info.Name, err
}

func (r *realClient) ResourceContainers(ctx context.Context) ([]container.Summary, error) {
	return r.cli.ContainerList(ctx, container.ListOptions{All: true})
}

func (r *realClient) ResourceVolumes(ctx context.Context) ([]*volume.Volume, error) {
	// /system/df only includes plain local volumes. Keep plugin volumes and
	// volumes with local mount options in inventory even when their size is unknown.
	list, listErr := r.cli.VolumeList(ctx, volume.ListOptions{})
	if len(list.Warnings) > 0 {
		listErr = errors.Join(listErr, errors.New("incomplete Docker volume list"))
	}
	usage, sizeErr := r.cli.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}})
	known := map[string]*volume.Volume{}
	for _, v := range list.Volumes {
		if v != nil {
			known[v.Name] = v
		}
	}
	for _, v := range usage.Volumes {
		if v == nil {
			continue
		}
		if orig := known[v.Name]; orig != nil {
			orig.UsageData = v.UsageData
		} else {
			list.Volumes = append(list.Volumes, v)
		}
	}
	return list.Volumes, errors.Join(listErr, sizeErr)
}

func (r *realClient) ResourceStorage(ctx context.Context, id string) (ContainerStorage, error) {
	info, _, err := r.cli.ContainerInspectWithRaw(ctx, id, true)
	if err != nil {
		// A storage driver can fail to size a layer while its log is still measurable.
		info, err = r.cli.ContainerInspect(ctx, id)
	}
	if err != nil {
		return ContainerStorage{}, err
	}
	if info.ContainerJSONBase == nil {
		return ContainerStorage{}, errors.New("container inspection unavailable")
	}
	result := ContainerStorage{Writable: info.SizeRw, LogPath: info.LogPath}
	if info.HostConfig != nil {
		result.LogDriver = info.HostConfig.LogConfig.Type
	}
	return result, nil
}

type RuntimeStats struct {
	Started, At time.Time
	Values      map[string]float64
	Counters    map[string]uint64
}

func (r *realClient) ResourceStats(ctx context.Context, id string) (RuntimeStats, error) {
	inspect, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return RuntimeStats{}, err
	}
	if inspect.ContainerJSONBase == nil || inspect.State == nil || !inspect.State.Running {
		return RuntimeStats{}, errors.New("container is not running")
	}
	started, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt)
	if err != nil {
		return RuntimeStats{}, err
	}
	stream, err := r.cli.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return RuntimeStats{}, err
	}
	defer stream.Body.Close()
	stats, err := decodeRuntimeStats(stream.Body, started)
	if err == nil && inspect.HostConfig != nil {
		switch {
		case inspect.HostConfig.Memory > 0:
			stats.Values["limit"] = float64(inspect.HostConfig.Memory)
		case inspect.HostConfig.Memory == 0:
			stats.Values["unlimited"] = 1
		}
	}
	return stats, err
}

func decodeRuntimeStats(in io.Reader, started time.Time) (RuntimeStats, error) {
	// Docker omits unsupported fields. Preserve that distinction for gauges and counters.
	var raw struct {
		Read   time.Time
		Memory struct {
			Usage *uint64
			Stats map[string]uint64
		} `json:"memory_stats"`
		CPU struct {
			Usage struct {
				Total *uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		PIDs     struct{ Current *uint64 } `json:"pids_stats"`
		Networks map[string]container.NetworkStats
		Block    container.BlkioStats `json:"blkio_stats"`
	}
	if err := json.NewDecoder(io.LimitReader(in, 2<<20)).Decode(&raw); err != nil {
		return RuntimeStats{}, err
	}
	if raw.Read.IsZero() || raw.Read.Before(started) {
		return RuntimeStats{}, errors.New("stats timestamp unavailable")
	}
	s := RuntimeStats{Started: started, At: raw.Read, Values: map[string]float64{}, Counters: map[string]uint64{}}
	if raw.Memory.Usage != nil {
		used := *raw.Memory.Usage
		inactive, ok := raw.Memory.Stats["total_inactive_file"]
		if !ok {
			inactive = raw.Memory.Stats["inactive_file"]
		}
		if inactive <= used {
			used -= inactive
		}
		s.Values["memory"] = float64(used)
	}
	if raw.PIDs.Current != nil {
		s.Values["pids"] = float64(*raw.PIDs.Current)
	}
	if raw.CPU.Usage.Total != nil {
		s.Counters["cpu"] = *raw.CPU.Usage.Total
	}
	if raw.Networks != nil {
		s.Counters["rx"], s.Counters["tx"] = 0, 0
		for _, net := range raw.Networks {
			s.Counters["rx"] += net.RxBytes
			s.Counters["tx"] += net.TxBytes
		}
	}
	if raw.Block.IoServiceBytesRecursive != nil {
		s.Counters["read"], s.Counters["write"] = 0, 0
		for _, entry := range raw.Block.IoServiceBytesRecursive {
			switch strings.ToLower(entry.Op) {
			case "read":
				s.Counters["read"] += entry.Value
			case "write":
				s.Counters["write"] += entry.Value
			}
		}
	}
	return s, nil
}

type PostgresSize struct {
	Kind, Name string
	Bytes      int64
}

// The query and psql options are fixed. The caller supplies the validated managed
// template's connection identity and addresses the inspected container by immutable ID.
func (r *realClient) PostgresSizes(ctx context.Context, id, user, database string) ([]PostgresSize, error) {
	const query = `SELECT json_build_object('Kind','postgres-database','Name',current_database(),'Bytes',pg_database_size(current_database()))
UNION ALL SELECT json_build_object('Kind','postgres-table','Name',name,'Bytes',bytes)
FROM (SELECT format('%I.%I', schemaname, relname) AS name, pg_total_relation_size(relid) AS bytes
FROM pg_stat_user_tables ORDER BY bytes DESC, name LIMIT 10) AS largest`
	created, err := r.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		User: "postgres", AttachStdout: true,
		Env: []string{"PGOPTIONS=-c search_path=pg_catalog -c default_transaction_read_only=on -c statement_timeout=3000 -c lock_timeout=500", "PGCONNECT_TIMEOUT=2"},
		Cmd: []string{"psql", "-X", "-A", "-t", "-w", "-v", "ON_ERROR_STOP=1", "--host=/var/run/postgresql", "--username=" + user, "--dbname=" + database, "-c", query},
	})
	if err != nil {
		return nil, err
	}
	attached, err := r.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	defer attached.Close()
	stop := context.AfterFunc(ctx, attached.Close)
	defer stop()
	output, truncated, err := ReadLogsBounded(attached.Reader, 16<<10)
	if err != nil || truncated {
		return nil, errors.New("PostgreSQL size result unavailable")
	}
	err = waitForExecCompletion(ctx, func(ctx context.Context) (container.ExecInspect, error) {
		return r.cli.ContainerExecInspect(ctx, created.ID)
	}, id)
	if err != nil {
		return nil, errors.New("PostgreSQL size query failed")
	}
	var rows []PostgresSize
	for line := range strings.Lines(output) {
		var row PostgresSize
		if json.Unmarshal([]byte(line), &row) != nil || row.Name == "" || row.Bytes < 0 || (row.Kind != "postgres-database" && row.Kind != "postgres-table") {
			return nil, errors.New("invalid PostgreSQL size result")
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 || rows[0].Kind != "postgres-database" {
		return nil, errors.New("PostgreSQL database size unavailable")
	}
	return rows, nil
}
