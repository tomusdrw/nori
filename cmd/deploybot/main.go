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

	ex := executor.New(st, executor.OSRunner{}, latest, 0)
	pl := poller.New(st, latest, ex, cfg.PollInterval)
	sched := scheduler.New(st, ex)

	a, err := auth.New(cfg.AdminPasswordHash, cfg.SessionKey)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pl.Run(ctx)
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
