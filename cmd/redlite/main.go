// Command redlite runs one red_lite_cache instance as a leader or a replica (REDLITE_ROLE).
//
// S0 scaffold: loads config, configures the logger, serves /health, and shuts down cleanly on
// SIGINT/SIGTERM. The WebSocket listener (S2), replica client (S4) and persistence (S5) mount here.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

func main() {
	cfg, err := config.Load(".")
	if err != nil {
		logger.L.Error("Config: load failed", "err", err)
		os.Exit(1)
	}
	logger.Configure(cfg.LogLevel, cfg.Environment)
	logger.Banner(cfg.Role, cfg.Port, cfg.LeaderAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","role":"` + cfg.Role + `"}`))
	})

	srv := &http.Server{
		Addr:              cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		logger.L.Info("Config: shutdown signal received")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.L.Error("Config: listen failed", "port", cfg.Port, "err", err)
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.L.Error("Config: shutdown failed", "err", err)
	}
	logger.L.Info("Config: stopped")
}
