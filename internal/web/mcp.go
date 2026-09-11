package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"deploybot/internal/docker"
	"deploybot/internal/envfile"
	"deploybot/internal/executor"
	"deploybot/internal/mcpauth"
	"deploybot/internal/store"
	"github.com/distribution/reference"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/robfig/cron/v3"
)

const mcpLogLimit = 256 * 1024

type mcpID struct {
	ServiceID int64 `json:"service_id" jsonschema:"Service ID"`
}
type mcpCreate struct {
	Name         string  `json:"name"`
	WatchedImage string  `json:"watched_image"`
	DeployScript string  `json:"deploy_script"`
	Policy       string  `json:"policy,omitempty"`
	CronExpr     string  `json:"cron_expr,omitempty"`
	HealthURL    string  `json:"health_url,omitempty"`
	EnvFile      *string `json:"env_file,omitempty" jsonschema:"Complete dotenv document, encrypted at rest"`
}
type mcpUpdate struct {
	ServiceID    int64   `json:"service_id"`
	WatchedImage *string `json:"watched_image,omitempty"`
	DeployScript *string `json:"deploy_script,omitempty"`
	Policy       *string `json:"policy,omitempty"`
	CronExpr     *string `json:"cron_expr,omitempty"`
	HealthURL    *string `json:"health_url,omitempty"`
	EnvFile      *string `json:"env_file,omitempty"`
}
type mcpHistory struct {
	ServiceID int64 `json:"service_id"`
	Limit     int   `json:"limit,omitempty" jsonschema:"Maximum deployments, defaults to 20, at most 100"`
}
type mcpDeployment struct {
	DeploymentID int64 `json:"deployment_id"`
	IncludeLogs  bool  `json:"include_logs,omitempty" jsonschema:"Explicitly include logs, which may contain secrets"`
}
type mcpLogs struct {
	ServiceID   int64  `json:"service_id"`
	ContainerID string `json:"container_id" jsonschema:"Container belonging to this service"`
	Tail        int    `json:"tail,omitempty" jsonschema:"Lines, defaults to 100, maximum 1000"`
}
type mcpEnv struct {
	ServiceID int64  `json:"service_id"`
	EnvFile   string `json:"env_file" jsonschema:"Complete replacement dotenv document"`
}

// addNoriTool puts authorization before all tool side effects. The OAuth HTTP
// middleware supplies the authenticated principal on every stateless request.
func addNoriTool[I any](server *mcp.Server, name, description, scope string, run func(context.Context, I) (any, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: scope == mcpauth.ScopeRead}}, func(ctx context.Context, _ *mcp.CallToolRequest, in I) (*mcp.CallToolResult, any, error) {
		if err := mcpauth.RequireScope(ctx, scope); err != nil {
			return nil, nil, err
		}
		out, err := run(ctx, in)
		if scope == mcpauth.ScopeWrite {
			identity, _ := mcpauth.FromContext(ctx)
			var serviceID int64
			switch v := any(in).(type) {
			case mcpID:
				serviceID = v.ServiceID
			case mcpUpdate:
				serviceID = v.ServiceID
			case mcpEnv:
				serviceID = v.ServiceID
			}
			log.Printf("mcp: client=%q tool=%s service_id=%d success=%t", identity.ClientID, name, serviceID, err == nil)
		}
		return nil, out, err
	})
}

func (s *Server) newMCPHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "nori", Version: "1.0.0"}, nil)
	addNoriTool(server, "list_services", "List service configurations without environment values.", mcpauth.ScopeRead, func(ctx context.Context, _ struct{}) (any, error) { return s.store.ListServices(ctx) })
	addNoriTool(server, "get_service", "Get a service configuration and its containers; environment values are excluded.", mcpauth.ScopeRead, func(ctx context.Context, in mcpID) (any, error) {
		svc, err := s.mcpService(ctx, in.ServiceID, false)
		if err != nil {
			return nil, err
		}
		containers, err := s.docker.ListByService(ctx, svc.Name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"service": svc, "containers": containers}, nil
	})
	addNoriTool(server, "create_service", "Create a service with a Bash deployment script. Scripts execute on the server with Docker access.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpCreate) (any, error) {
		if in.Policy == "" {
			in.Policy = string(store.PolicyManual)
		}
		svc := &store.Service{Name: in.Name, WatchedImage: in.WatchedImage, DeployScript: in.DeployScript, Policy: store.Policy(in.Policy), CronExpr: in.CronExpr, HealthURL: in.HealthURL}
		if err := validateMCPService(ctx, svc, in.EnvFile); err != nil {
			return nil, err
		}
		if err := s.store.SaveServiceConfig(ctx, svc, in.EnvFile, nil); err != nil {
			return nil, errors.New("could not create service; check that its name is unique")
		}
		return svc, nil
	})
	addNoriTool(server, "update_service", "Update supplied configuration fields; omitted fields are preserved and names are immutable. Scripts execute on the server with Docker access.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpUpdate) (any, error) {
		svc, err := s.mcpService(ctx, in.ServiceID, true)
		if err != nil {
			return nil, err
		}
		previous := *svc
		if in.WatchedImage != nil {
			svc.WatchedImage = *in.WatchedImage
		}
		if in.DeployScript != nil {
			svc.DeployScript = *in.DeployScript
		}
		if in.Policy != nil {
			svc.Policy = store.Policy(*in.Policy)
		}
		if in.CronExpr != nil {
			svc.CronExpr = *in.CronExpr
		}
		if in.HealthURL != nil {
			svc.HealthURL = *in.HealthURL
		}
		if err := validateMCPService(ctx, svc, in.EnvFile); err != nil {
			return nil, err
		}
		if err := s.store.SaveServiceConfig(ctx, svc, in.EnvFile, &previous); err != nil {
			if errors.Is(err, store.ErrServiceConflict) {
				return nil, err
			}
			return nil, errors.New("could not update service")
		}
		return svc, nil
	})
	for _, action := range []string{"delete", "start", "stop", "deploy"} {
		addNoriTool(server, action+"_service", map[string]string{"delete": "Delete service configuration and deployment history; does not remove containers.", "start": "Start existing containers belonging to a service.", "stop": "Stop all containers belonging to a service.", "deploy": "Run the service deployment script asynchronously; returns deployment ID."}[action], mcpauth.ScopeWrite, func(ctx context.Context, in mcpID) (any, error) {
			svc, err := s.mcpService(ctx, in.ServiceID, true)
			if err != nil {
				return nil, err
			}
			switch action {
			case "delete":
				err = s.store.DeleteService(ctx, svc.ID)
			case "start":
				err = s.docker.StartByService(ctx, svc.Name)
			case "stop":
				err = s.docker.StopByService(ctx, svc.Name)
			case "deploy":
				id, e := s.executor.Deploy(ctx, svc.ID, store.TriggerManual)
				if e != nil {
					return nil, errors.New("deployment could not start")
				}
				return map[string]any{"deployment_id": id}, nil
			}
			if err != nil {
				return nil, fmt.Errorf("could not %s service", action)
			}
			return map[string]any{"service_id": svc.ID, "success": true}, nil
		})
	}
	addNoriTool(server, "list_deployments", "Recent deployment metadata without logs.", mcpauth.ScopeRead, func(ctx context.Context, in mcpHistory) (any, error) {
		if _, err := s.mcpService(ctx, in.ServiceID, false); err != nil {
			return nil, err
		}
		if in.Limit == 0 {
			in.Limit = 20
		}
		if in.Limit < 1 || in.Limit > 100 {
			return nil, errors.New("limit must be 1..100")
		}
		return s.store.ListDeploymentSummaries(ctx, in.ServiceID, in.Limit)
	})
	addNoriTool(server, "get_deployment", "Deployment details; optional logs may contain sensitive application output and are limited to 256 KiB.", mcpauth.ScopeRead, func(ctx context.Context, in mcpDeployment) (any, error) {
		if in.DeploymentID <= 0 {
			return nil, errors.New("invalid deployment_id")
		}
		limit := 0
		if in.IncludeLogs {
			limit = mcpLogLimit + 1
		}
		d, err := s.store.GetDeploymentWithLogLimit(ctx, in.DeploymentID, limit)
		if err != nil {
			return nil, err
		}
		truncated := false
		if !in.IncludeLogs {
			d.Log = ""
		} else if len(d.Log) > mcpLogLimit {
			d.Log = d.Log[len(d.Log)-mcpLogLimit:]
			truncated = true
		}
		return map[string]any{"deployment": d, "logs_truncated": truncated}, nil
	})
	addNoriTool(server, "get_container_logs", "Read bounded container logs. Output can include application secrets. Maximum 1000 lines and 256 KiB.", mcpauth.ScopeRead, func(ctx context.Context, in mcpLogs) (any, error) {
		svc, err := s.mcpService(ctx, in.ServiceID, false)
		if err != nil {
			return nil, err
		}
		if in.Tail == 0 {
			in.Tail = 100
		}
		if in.Tail < 1 || in.Tail > 1000 {
			return nil, errors.New("tail must be 1..1000")
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cs, err := s.docker.ListByService(ctx, svc.Name)
		if err != nil {
			return nil, errors.New("could not list containers")
		}
		found := false
		for _, c := range cs {
			if c.ID == in.ContainerID {
				found = true
			}
		}
		if !found {
			return nil, errors.New("container does not belong to this service")
		}
		rc, err := s.docker.Logs(ctx, in.ContainerID, in.Tail)
		if err != nil {
			return nil, errors.New("could not read container logs")
		}
		defer rc.Close()
		output, truncated, err := docker.ReadLogsBounded(rc, mcpLogLimit)
		if err != nil {
			return nil, errors.New("could not decode container logs")
		}
		return map[string]any{"logs": output, "truncated": truncated}, nil
	})
	addNoriTool(server, "get_service_environment", "Explicitly reveal a service's plaintext dotenv document. Requires nori:secrets in addition to nori:read.", mcpauth.ScopeRead, func(ctx context.Context, in mcpID) (any, error) {
		if err := mcpauth.RequireScope(ctx, mcpauth.ScopeSecrets); err != nil {
			return nil, err
		}
		svc, err := s.mcpService(ctx, in.ServiceID, false)
		if err != nil {
			return nil, err
		}
		if svc.IsSelf {
			return nil, errors.New("managed self-service environment is unavailable through MCP")
		}
		env, err := s.store.GetEnvFile(ctx, svc.ID)
		if err != nil {
			return nil, errors.New("could not read environment")
		}
		return map[string]any{"env_file": env}, nil
	})
	addNoriTool(server, "set_service_environment", "Replace a service's entire dotenv document, encrypted at rest. Does not return the values.", mcpauth.ScopeWrite, func(ctx context.Context, in mcpEnv) (any, error) {
		svc, err := s.mcpService(ctx, in.ServiceID, true)
		if err != nil {
			return nil, err
		}
		in.EnvFile = executor.NormalizeNewlines(in.EnvFile)
		if len(in.EnvFile) > 128*1024 {
			return nil, errors.New("environment must be at most 128 KiB")
		}
		if _, err := envfile.Parse(in.EnvFile); err != nil {
			return nil, errors.New("invalid dotenv syntax")
		}
		if err := s.store.SetEnvFile(ctx, svc.ID, in.EnvFile); err != nil {
			return nil, errors.New("could not save environment")
		}
		return map[string]any{"success": true}, nil
	})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20})
}

func (s *Server) mcpService(ctx context.Context, id int64, mutation bool) (*store.Service, error) {
	if id <= 0 {
		return nil, errors.New("invalid service_id")
	}
	svc, err := s.store.GetService(ctx, id)
	if err != nil {
		return nil, err
	}
	if mutation && svc.IsSelf {
		return nil, errors.New("managed self-service cannot be changed through MCP; use instance settings")
	}
	return svc, nil
}

var mcpServiceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func validateMCPService(ctx context.Context, svc *store.Service, env *string) error {
	if !mcpServiceName.MatchString(svc.Name) || svc.Name == store.SelfServiceName {
		return errors.New("invalid or reserved service name")
	}
	if strings.TrimSpace(svc.WatchedImage) == "" || len(svc.WatchedImage) > 512 {
		return errors.New("watched_image is required and must be at most 512 bytes")
	}
	if _, err := reference.ParseNormalizedNamed(svc.WatchedImage); err != nil {
		return errors.New("invalid watched_image reference")
	}
	switch svc.Policy {
	case store.PolicyManual, store.PolicyImmediate:
	case store.PolicyScheduled:
		if _, err := cron.ParseStandard(svc.CronExpr); err != nil {
			return errors.New("invalid cron expression")
		}
	default:
		return errors.New("policy must be manual, immediate or scheduled")
	}
	if strings.TrimSpace(svc.DeployScript) == "" || len(svc.DeployScript) > 128*1024 {
		return errors.New("deploy_script is required and must be at most 128 KiB")
	}
	svc.DeployScript = executor.NormalizeNewlines(svc.DeployScript)
	form := serviceForm(svc)
	if env != nil {
		if len(*env) > 128*1024 {
			return errors.New("environment must be at most 128 KiB")
		}
		*env = executor.NormalizeNewlines(*env)
		form.EnvFile = *env
	}
	if err := validateServiceForm(ctx, form); err != nil {
		return errors.New("invalid service configuration: check Bash syntax, dotenv syntax and health URL")
	}
	return nil
}
