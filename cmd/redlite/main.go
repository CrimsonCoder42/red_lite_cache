// Command redlite runs one red_lite_cache instance as a leader or a replica (REDLITE_ROLE).
//
// It loads config, configures the logger, builds the instance (internal/app), listens on
// REDLITE_PORT, prints the one banner line (K8) and serves until SIGINT/SIGTERM, then shuts down
// within REDLITE_SHUTDOWN_TIMEOUT. Any start-up or serve failure exits 1 after an Error log.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/CrimsonCoder42/red_lite_cache/internal/app"
	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

func main() {
	if err := run(); err != nil {
		logger.L.Error("Config: redlite failed", "err", err)
		os.Exit(1)
	}
}

// run is main without the exit, so every deferred cleanup runs before os.Exit.
//
// after advo_cache/main.go:28-262: LoadConfig, wiring, a 150-line banner and ListenAndServe with no
// shutdown became config.Load → app.New → one banner → app.Run under a signal context.
func run() error {
	cfg, err := config.Load(".")
	if err != nil {
		return err
	}
	logger.Configure(cfg.LogLevel, cfg.Environment)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Port)
	if err != nil {
		return fmt.Errorf("redlite: listen %s: %w", cfg.Port, err)
	}
	logger.Banner(cfg.Role, ln.Addr().String(), cfg.LeaderAddr, a.InstanceID())
	return a.Run(ctx, ln)
}
