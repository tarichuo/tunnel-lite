package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"tunnel-lite/internal/agent"
	"tunnel-lite/internal/config"
)

func main() {
	path := flag.String("c", "agent.yaml", "Agent YAML configuration")
	flag.Parse()
	cfg, err := config.LoadAgent(*path)
	if err != nil {
		slog.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	a, err := agent.New(cfg)
	if err != nil {
		slog.Error("Agent initialization failed", "error", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = a.Run(ctx); err != nil {
		slog.Error("Agent failed", "error", err)
		os.Exit(1)
	}
}
