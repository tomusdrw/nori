package docker

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

const ServiceLabel = "deploybot.service"

type Container struct {
	ID     string
	Name   string
	Image  string
	Digest string
	State  string
}

type Client interface {
	ListByService(ctx context.Context, service string) ([]Container, error)
	Logs(ctx context.Context, containerID string, tail int) (io.ReadCloser, error)
	StartByService(ctx context.Context, service string) error
	StopByService(ctx context.Context, service string) error
}

type realClient struct{ cli *client.Client }

func New(host string) (Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append([]client.Opt{client.WithHost(host)}, opts...)
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	return &realClient{cli: cli}, nil
}

func (r *realClient) ListByService(ctx context.Context, service string) ([]Container, error) {
	f := filters.NewArgs(filters.Arg("label", fmt.Sprintf("%s=%s", ServiceLabel, service)))
	list, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		digest := ""
		if insp, _, err := r.cli.ImageInspectWithRaw(ctx, c.ImageID); err == nil && len(insp.RepoDigests) > 0 {
			if i := strings.Index(insp.RepoDigests[0], "@"); i >= 0 {
				digest = insp.RepoDigests[0][i+1:]
			}
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Container{ID: c.ID, Name: name, Image: c.Image, Digest: digest, State: c.State})
	}
	return out, nil
}

func (r *realClient) Logs(ctx context.Context, containerID string, tail int) (io.ReadCloser, error) {
	return r.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
}

func (r *realClient) StartByService(ctx context.Context, service string) error {
	cs, err := r.ListByService(ctx, service)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.State != "running" {
			if err := r.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *realClient) StopByService(ctx context.Context, service string) error {
	cs, err := r.ListByService(ctx, service)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.State == "running" {
			if err := r.cli.ContainerStop(ctx, c.ID, container.StopOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}
