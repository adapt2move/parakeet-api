// Command parakeet-api serves the transcription API and its internal worker queue.
//
// Usage:
//
//	parakeet-api              serve, configured from the environment
//	parakeet-api healthcheck  exit 0 when the local server is ready, 1 otherwise
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/adapt2move/parakeet-api/internal/api"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(api.Healthcheck(os.Getenv("LISTEN_ADDR")))
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: parakeet-api [healthcheck]")
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := api.LoadSettings(os.Getenv)
	if err != nil {
		log.Error("invalid_settings", "error", err.Error())
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := api.Run(ctx, cfg, log); err != nil {
		log.Error("api_failed", "error", err.Error())
		os.Exit(1)
	}
}
