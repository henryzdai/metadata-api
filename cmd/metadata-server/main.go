package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"metadata-api/internal/server"
)

func main() { os.Exit(run()) }

func run() int {
	cfg, err := server.ParseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err != nil {
		logger.Error("startup_failed", "error", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, cfg, os.Stdout, os.Stderr); err != nil {
		logger.Error("server_stopped", "error", err.Error())
		return 1
	}
	return 0
}
