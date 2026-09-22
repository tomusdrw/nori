package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"nori/internal/envcompat"
	"nori/internal/launcher"
)

func runLauncherCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("launcher command is required")
	}
	l := launcher.New()
	if dir := envcompat.Get("NORI_CONFIG_DIR"); dir != "" {
		l.ConfigDir = dir
	}

	switch args[0] {
	case "up":
		return runUp(ctx, l, args[1:])
	case "update":
		return runUpdate(ctx, l, args[1:])
	case "rollback":
		if len(args) != 1 {
			return errors.New("usage: nori rollback")
		}
		return l.Rollback(ctx)
	default:
		return fmt.Errorf("unknown launcher command %q", args[0])
	}
}

func runUp(ctx context.Context, l *launcher.Launcher, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	defaultImage := envcompat.Get("NORI_SELF_IMAGE")
	if defaultImage == "" {
		defaultImage = envcompat.Get("NORI_IMAGE")
	}
	image := fs.String("image", defaultImage, "nori image to run (required on first boot)")
	configVolume := fs.String("config-volume", envOr("NORI_CONFIG_VOLUME", launcher.DefaultConfigVolume), "external Docker volume mounted at /config")
	containerName := fs.String("container-name", envOr("NORI_SELF_CONTAINER", launcher.DefaultContainer), "nori container name")
	dataVolume := fs.String("data-volume", envOr("NORI_DATA_VOLUME", launcher.DefaultDataVolume), "external Docker volume mounted at /data")
	noPort := fs.Bool("no-port", false, "do not publish a host port (for reverse proxies)")
	network := fs.String("network", "", "Docker network for the nori container")
	encryptionKey := fs.String("key", envcompat.Get("NORI_KEY"), "existing base64 encryption key (migration only)")
	sessionKey := fs.String("session-key", envcompat.Get("NORI_SESSION_KEY"), "existing base64 session key (migration only)")
	adminHash := fs.String("admin-password-hash", envcompat.Get("NORI_ADMIN_HASH"), "bcrypt admin password hash for non-interactive bootstrap")
	var ports stringList
	fs.Var(&ports, "port", "host:container port mapping (repeatable)")
	var volumes stringList
	fs.Var(&volumes, "volume", "extra host bind or named volume mount (src:dst[:opts], repeatable)")
	var environment stringList
	fs.Var(&environment, "env", "environment variable for nori (KEY=VALUE, repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if len(ports) == 0 && !*noPort {
		if port := envcompat.Get("NORI_PORT"); port != "" {
			ports = append(ports, port+":8080")
		}
	}
	return l.Up(ctx, launcher.UpOptions{
		Image:             *image,
		ConfigVolume:      *configVolume,
		ContainerName:     *containerName,
		DataVolume:        *dataVolume,
		Ports:             ports,
		NoPort:            *noPort,
		Network:           *network,
		Volumes:           volumes,
		Environment:       environment,
		EncryptionKey:     *encryptionKey,
		SessionKey:        *sessionKey,
		AdminPasswordHash: *adminHash,
	})
}

func runUpdate(ctx context.Context, l *launcher.Launcher, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	digest := fs.String("target-digest", "", "target sha256 digest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *digest == "" {
		return errors.New("--target-digest is required")
	}
	return l.Update(ctx, *digest)
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func envOr(key, fallback string) string {
	if value := envcompat.Get(key); value != "" {
		return value
	}
	return fallback
}
