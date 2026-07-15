package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"deploybot/internal/config"
	"deploybot/internal/docker"
	"deploybot/internal/registry"
	"deploybot/internal/store"
	"deploybot/internal/web"
)

func main() {
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

	latest := func(ctx context.Context, image string) (string, error) {
		return registry.LatestDigest(image)
	}

	srv := web.NewServer(st, dk, latest)
	log.Printf("listening on %s", cfg.ListenAddr)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, srv))
}
