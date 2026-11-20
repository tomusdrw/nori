package web

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"nori/internal/envfile"
	"nori/internal/mcpauth"
	"nori/internal/store"
)

type mcpResourceUpdate struct {
	ServiceID       int64               `json:"service_id"`
	UsageBytes      *int64              `json:"usage_bytes,omitempty" jsonschema:"Storage alert threshold in bytes. Zero disables it."`
	GrowthBytes     *int64              `json:"growth_bytes,omitempty" jsonschema:"24-hour growth alert threshold in bytes. Zero disables it."`
	Runtime         *map[string]float64 `json:"runtime,omitempty" jsonschema:"Replace all runtime alert thresholds. Keys: cpu (percent), memory (bytes), rx, tx, read, write (bytes per second), pids (count). Empty object clears them."`
	Directories     *[]string           `json:"directories,omitempty" jsonschema:"Replace the list of monitored absolute directories. Empty list clears it."`
	PostgresDetails *bool               `json:"postgres_details,omitempty" jsonschema:"Collect database sizes for services using the managed PostgreSQL template."`
}

func (in mcpResourceUpdate) apply(limits *store.ResourceLimits) {
	if in.UsageBytes != nil {
		limits.UsageBytes = *in.UsageBytes
	}
	if in.GrowthBytes != nil {
		limits.GrowthBytes = *in.GrowthBytes
	}
	if in.Runtime != nil {
		limits.Runtime = *in.Runtime
	}
	if in.Directories != nil {
		limits.Directories = *in.Directories
	}
	if in.PostgresDetails != nil {
		limits.PostgresDetails = *in.PostgresDetails
	}
}

func mcpResourceSettings(limits store.ResourceLimits) map[string]any {
	runtime := limits.Runtime
	if runtime == nil {
		runtime = map[string]float64{}
	}
	return map[string]any{
		"usage_bytes": limits.UsageBytes, "growth_bytes": limits.GrowthBytes,
		"runtime": runtime, "directories": append([]string{}, limits.Directories...),
		"postgres_details": limits.PostgresDetails,
	}
}

type mcpConfigHistory struct {
	ServiceID int64  `json:"service_id"`
	Kind      string `json:"kind" jsonschema:"script or env"`
}

type mcpConfigRevision struct {
	ServiceID int64  `json:"service_id"`
	Kind      string `json:"kind" jsonschema:"script or env"`
	Version   int64  `json:"version"`
}

type mcpApplyConfigRevision struct {
	ServiceID             int64  `json:"service_id" jsonschema:"Destination service ID."`
	SourceServiceID       int64  `json:"source_service_id,omitempty" jsonschema:"Source service ID. Defaults to service_id."`
	Kind                  string `json:"kind" jsonschema:"script or env"`
	Version               int64  `json:"version"`
	ExpectedConfigVersion int64  `json:"expected_config_version" jsonschema:"Current destination configuration generation from get_service."`
}

type mcpResourceHistory struct {
	ServiceID int64  `json:"service_id"`
	Range     string `json:"range,omitempty" jsonschema:"1h, 24h (default), 7d, or 30d"`
}

func mcpResourceValues(values map[string]resourceValue) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		var amount any
		if value.known {
			amount = value.number
		}
		out[key] = map[string]any{"value": amount, "unlimited": value.unlimited,
			"partial": value.incompleteInventory || len(value.missing) != 0}
	}
	return out
}

func (s *Server) addMCPServiceDiagnostics(server *mcp.Server) {
	addNoriTool(s, server, "get_instance_resources", "Read instance-wide resource totals and usage grouped by service, including unattributed usage.", mcpauth.ScopeRead, func(ctx context.Context, _ struct{}) (any, error) {
		view, err := s.loadResourceView(ctx, time.Now())
		if err != nil {
			return nil, errors.New("could not load resource inventory")
		}
		services := make([]map[string]any, 0, len(view.services))
		for _, id := range slices.Sorted(maps.Keys(view.services)) {
			name := view.services[id]
			services = append(services, map[string]any{"service_id": id, "name": name,
				"runtime": mcpResourceValues(view.runtimeFor(id)), "storage": mcpResourceValues(view.storageFor(id))})
		}
		storageSources := view.sourceTable(-1, false).rows
		sources := make([]map[string]any, 0, len(storageSources))
		for _, source := range storageSources {
			sources = append(sources, map[string]any{"name": source.name, "detail": source.detail, "storage": mcpResourceValues(source.values)})
		}
		return map[string]any{"services": services,
			"storage_sources": sources, "unattributed_containers": view.runtime.byService[0],
			"runtime_sampled_at": view.runtime.at.UTC(), "storage_sampled_at": view.storage.at.UTC(),
			"total_runtime": mcpResourceValues(view.runtimeFor(-1)), "total_storage": mcpResourceValues(view.storageFor(-1)),
			"unattributed_runtime": mcpResourceValues(view.runtimeFor(0)), "unattributed_storage": mcpResourceValues(view.storageFor(0))}, nil
	})
	addNoriTool(s, server, "get_service_resource_history", "Read historical runtime and storage samples and deployment markers for a service. Returns up to 48 observations per stream.", mcpauth.ScopeRead, func(ctx context.Context, in mcpResourceHistory) (any, error) {
		if _, err := s.mcpService(ctx, in.ServiceID, false); err != nil {
			return nil, err
		}
		key, window := resourceWindow(in.Range)
		if in.Range != "" && key != in.Range {
			return nil, errors.New("range must be 1h, 24h, 7d, or 30d")
		}
		now := time.Now().UTC()
		start := now.Add(-window)
		out := map[string]any{"start": start, "end": now}
		for _, stream := range []string{"runtime", "storage"} {
			history, err := s.store.ResourceChartHistory(ctx, stream, start, now, 48, in.ServiceID)
			if err != nil {
				return nil, errors.New("could not load resource history")
			}
			out[stream] = history
		}
		deployments, err := s.store.ListResourceDeployments(ctx, in.ServiceID, start, now)
		if err != nil {
			return nil, errors.New("could not load deployment markers")
		}
		out["deployments"] = deployments
		return out, nil
	})
	addNoriTool(s, server, "get_service_config_revision", "Read a saved script revision or the redacted dotenv template of an environment revision. Use apply_service_config_revision to apply the whole reviewed revision.", mcpauth.ScopeRead, func(ctx context.Context, in mcpConfigRevision) (any, error) {
		if !validHistoryKind(in.Kind) || in.Version <= 0 {
			return nil, errors.New("kind must be script or env and version must be positive")
		}
		if _, err := s.mcpService(ctx, in.ServiceID, false); err != nil {
			return nil, err
		}
		item, err := s.store.GetConfigRevision(ctx, in.ServiceID, in.Kind, in.Version)
		if err != nil {
			return nil, errors.New("configuration revision unavailable")
		}
		if in.Kind == "env" {
			item.Content, err = envfile.Template(item.Content)
			if err != nil {
				return nil, errors.New("could not redact environment revision")
			}
		}
		return item, nil
	})

	addNoriTool(s, server, "apply_service_config_revision", "Apply a reviewed script or environment revision to an existing service without deploying it. Environment revisions restore archived values without returning them and require nori:secrets as well as nori:write.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpApplyConfigRevision) (any, error) {
		if !validHistoryKind(in.Kind) || in.Version <= 0 || in.ExpectedConfigVersion <= 0 {
			return nil, errors.New("kind must be script or env; version and expected_config_version must be positive")
		}
		if in.Kind == "env" {
			if err := mcpauth.RequireScope(ctx, mcpauth.ScopeSecrets); err != nil {
				return nil, err
			}
		}
		target, err := s.mcpService(ctx, in.ServiceID, true)
		if err != nil {
			return nil, err
		}
		sourceID := in.SourceServiceID
		if sourceID == 0 {
			sourceID = target.ID
		}
		if _, err := s.mcpService(ctx, sourceID, true); err != nil {
			return nil, err
		}
		saved, err := s.store.GetConfigRevision(ctx, sourceID, in.Kind, in.Version)
		if err != nil {
			return nil, errors.New("configuration revision unavailable")
		}
		expected := *target
		expected.ConfigVersion = in.ExpectedConfigVersion
		write := ordinaryWrite{Service: target, Previous: &expected}
		if in.Kind == "script" {
			if target.DeploymentMode != store.DeploymentModeCustom && target.DeploymentMode != store.DeploymentModeManualScript {
				return nil, errors.New("script revisions require a Custom or manual script destination service")
			}
			target.DeployScript = saved.Content
		} else {
			write.Environment = &saved.Content
		}
		if _, err := s.saveOrdinaryService(ctx, write); err != nil {
			if errors.Is(err, store.ErrServiceConflict) {
				return nil, store.ErrServiceConflict
			}
			return nil, errors.New("could not apply revision; check the destination configuration")
		}
		return map[string]int64{"service_id": target.ID, "config_version": target.ConfigVersion}, nil
	})

	addNoriTool(s, server, "get_service_resources",
		"Read resource alert settings and the latest recorded runtime and storage measurements for this service. Missing samples or metrics indicate unavailable data.",
		mcpauth.ScopeRead, func(ctx context.Context, in mcpID) (any, error) {
			svc, err := s.mcpService(ctx, in.ServiceID, false)
			if err != nil {
				return nil, err
			}
			limits, err := s.store.ResourceLimits(ctx, svc.ID)
			if err != nil {
				return nil, errors.New("could not read resource settings")
			}
			result := map[string]any{"settings": mcpResourceSettings(limits)}
			for _, stream := range []string{"runtime", "storage"} {
				sample, err := s.store.LatestResourceSnapshot(ctx, stream, time.Time{})
				if err != nil {
					return nil, errors.New("could not read resource measurements")
				}
				result[stream] = nil
				if sample.At.IsZero() {
					continue
				}
				readings := make([]store.ResourceReading, 0)
				for _, reading := range sample.Readings {
					if slices.Contains(reading.Services, svc.ID) {
						readings = append(readings, reading)
					}
				}
				result[stream] = map[string]any{"sampled_at": sample.At.UTC(), "readings": readings}
			}
			return result, nil
		})

	addNoriTool(s, server, "update_service_resource_settings",
		"Update resource alert thresholds and collection settings. Omitted fields are preserved. Thresholds report alerts and do not cap container resource usage.",
		mcpauth.ScopeWrite, func(ctx context.Context, in mcpResourceUpdate) (any, error) {
			svc, err := s.mcpService(ctx, in.ServiceID, true)
			if err != nil {
				return nil, err
			}
			if in.PostgresDetails != nil && *in.PostgresDetails && svc.DeploymentMode != store.DeploymentModePostgres {
				return nil, errors.New("postgres_details requires a managed PostgreSQL service")
			}
			limits, err := s.store.UpdateResourceLimits(ctx, svc.ID, in.apply)
			if err != nil {
				return nil, err
			}
			return mcpResourceSettings(limits), nil
		})

	addNoriTool(s, server, "list_service_config_revisions",
		"List metadata for up to 100 recent deployment script or environment revisions. Returns version numbers and timestamps.",
		mcpauth.ScopeRead, func(ctx context.Context, in mcpConfigHistory) (any, error) {
			if !validHistoryKind(in.Kind) {
				return nil, errors.New("kind must be script or env")
			}
			svc, err := s.mcpService(ctx, in.ServiceID, false)
			if err != nil {
				return nil, err
			}
			revisions, err := s.store.ListRecentConfigRevisions(ctx, svc.ID, in.Kind, 100)
			if err != nil {
				return nil, errors.New("could not read configuration history")
			}
			return map[string]any{"revisions": revisions}, nil
		})
}
