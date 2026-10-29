// Package logger is the one logger. Package code calls L; cmd/redlite configures it once from the
// REDLITE_LOG_LEVEL config value and prints the startup banner through Banner.
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// L is the process logger. It is a text handler at Info until Configure runs.
var L = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Configure replaces L with a handler at the named level: debug, info, warn or error.
// An unknown name is treated as info. JSON output when environment is "production".
func Configure(level, environment string) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	if environment == "production" {
		L = slog.New(slog.NewJSONHandler(os.Stdout, opts))
		return
	}
	L = slog.New(slog.NewTextHandler(os.Stdout, opts))
}

// Banner logs the one startup line a running instance identifies itself by: role, port and, for a
// replica, the leader it follows. It is the only banner; the protocol how-to lives in doc/protocol/wire.md.
func Banner(role, port, leader string) {
	if leader == "" {
		L.Info("Config: redlite starting", "role", role, "port", port)
		return
	}
	L.Info("Config: redlite starting", "role", role, "port", port, "leader", leader)
}
