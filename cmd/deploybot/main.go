package main

import (
	"log"

	"deploybot/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	_ = cfg
	log.Println("deploybot: config loaded")
}
