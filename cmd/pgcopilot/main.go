package main

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

func main() {
	// Load .env file if present; ignore error in production where env vars
	// are injected by the runtime (e.g. Docker, systemd).
	if err := godotenv.Load(); err != nil {
		if _, statErr := os.Stat(".env"); statErr == nil {
			log.Fatalf("Error loading .env file: %v", err)
		}
	}

	Execute()
}
