package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"tunnel-lite/internal/cert"
	"tunnel-lite/internal/config"
	"tunnel-lite/internal/logging"
	"tunnel-lite/internal/server"
)

func main() {
	path := flag.String("c", "server.yaml", "server YAML configuration")
	initialize := flag.Bool("init", false, "create private configuration and TLS certificates (never overwrites)")
	dir := flag.String("dir", ".", "initialization output directory")
	host := flag.String("public-host", "127.0.0.1", "public IP included in generated server certificate")
	flag.Parse()
	if *initialize {
		if err := cert.Init(*dir, *host); err != nil {
			slog.Error("initialization failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("Created %s/server.yaml and TLS certificates. Read admin_token from the private config file.\n", *dir)
		return
	}
	cfg, err := config.LoadServer(*path)
	if err != nil {
		slog.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	logs, err := logging.Open(cfg.Logs.Path, int64(cfg.Logs.MaxSizeMB)*1024*1024, cfg.Logs.Backups)
	if err != nil {
		slog.Error("log initialization failed", "error", err)
		os.Exit(1)
	}
	defer logs.Close()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stderr, logs), nil)))
	s, err := server.New(cfg)
	if err != nil {
		slog.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = s.Run(ctx); err != nil {
		s.Close()
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
