package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"deploybot/internal/auth"
	"deploybot/internal/config"
	"deploybot/internal/docker"
	"deploybot/internal/executor"
	monitorpkg "deploybot/internal/monitor"
	"deploybot/internal/notify"
	"deploybot/internal/poller"
	"deploybot/internal/registry"
	"deploybot/internal/scheduler"
	"deploybot/internal/store"
	terminalsession "deploybot/internal/terminal"
	"deploybot/internal/web"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "up" || os.Args[1] == "update" || os.Args[1] == "rollback") {
		if err := runLauncherCommand(context.Background(), os.Args[1:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if len(os.Args) < 3 {
			log.Fatal("usage: deploybot hash-password <password>")
		}
		hash, err := auth.HashPassword(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(hash)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	if len(os.Args) > 1 && os.Args[1] == "seed-demo" {
		if err := seedDemo(context.Background(), st); err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Println("seeded demo service")
		return
	}

	dk, err := docker.New(cfg.DockerHost)
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	if err := initializeSelfService(context.Background(), st, dk); err != nil {
		log.Printf("self-update: %v", err)
	}

	latest := func(ctx context.Context, image string) (string, error) {
		return registry.LatestDigest(image)
	}
	// Build a single notifier instance to share with monitor.
	nf := buildNotifier(cfg)
	ex := executor.New(st, executor.OSRunner{}, latest, 0)
	ex.SetNotifier(nf)
	ex.SetBotName(st.BotName(context.Background()))
	pl := poller.New(st, latest, ex, cfg.PollInterval)
	sched := scheduler.New(st, ex)

	// Initialize monitor with the same notifier instance as executor
	mon := monitorpkg.New(st, dk, nf, cfg.MonitorInterval)

	a, err := auth.New(cfg.AdminPasswordHash, cfg.SessionKey)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pl.Run(ctx)
	go mon.Run(ctx)
	if err := sched.Start(ctx); err != nil {
		log.Fatalf("scheduler: %v", err)
	}
	defer sched.Stop()

	term := terminalsession.New("deploybot", cfg.TerminalDir)
	srv := web.NewServer(st, dk, ex, pl, a, term)
	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: srv}

	go func() {
		log.Printf("listening on %s", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cancel()
	_ = httpSrv.Shutdown(context.Background())
}

// buildNotifier returns the service-down notifier chosen by configuration.
// When Twilio is not configured, the no-op notifier keeps the app behaving
// exactly as before. When fully configured, the Twilio notifier is wrapped
// in LogFailures so a Twilio outage can never fail a deploy.
func buildNotifier(cfg config.Config) notify.Notifier {
	if !cfg.Twilio.Enabled() {
		return notify.Noop{}
	}
	return &notify.LogFailures{Inner: notify.NewTwilio(
		cfg.Twilio.AccountSID,
		cfg.Twilio.AuthToken,
		cfg.Twilio.From,
		cfg.Twilio.To,
	)}
}
