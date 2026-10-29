// Package logger is the one logger. Package code calls L; cmd/redlite configures it once from the
// REDLITE_LOG_LEVEL config value and prints the startup banner through Banner.
package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// L is the process logger. It is a text handler at Info until Configure runs.
var L = New(os.Stdout, slog.LevelInfo, false)

// masked is the set of attribute keys whose value never reaches a log line, whatever the caller
// passed: tenant data, credentials and connection strings. A safety net under the "IDs only" rule
// (.claude/rules/backend/go-logging.md), not a substitute for it.
var masked = map[string]bool{
	"password":      true,
	"password_hash": true,
	"value":         true,
	"key_hash":      true,
	"api_key":       true,
	"database_url":  true,
}

// mask replaces the value of a masked attribute with "***".
// after code-heim go_26_slog: ReplaceAttr masking, extended from password-like keys to every key
// that can carry tenant data or a credential.
func mask(_ []string, a slog.Attr) slog.Attr {
	if masked[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "***")
	}
	return a
}

// New builds a logger writing to w at level, JSON when jsonOut, with the masking ReplaceAttr.
// Tests use it to capture output; package code uses L.
func New(w io.Writer, level slog.Level, jsonOut bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: mask}
	if jsonOut {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// ParseLevel maps debug, info, warn or error to a slog level. An unknown name is info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Configure replaces L with a handler at the named level: debug, info, warn or error.
// An unknown name is treated as info. JSON output when environment is "production".
func Configure(level, environment string) {
	L = New(os.Stdout, ParseLevel(level), environment == "production")
}

// Banner logs the one startup line a running instance identifies itself by: role, port and, for a
// replica, the leader it follows. It is the only banner; the protocol how-to lives in doc/protocol/wire.md.
// after advo_cache/main.go:95-260: the 150-line how-to became this one line (K8).
func Banner(role, port, leader, instanceID string) {
	if leader == "" {
		L.Info("Config: redlite starting", "role", role, "port", port, "instance", instanceID)
		return
	}
	L.Info("Config: redlite starting", "role", role, "port", port, "leader", leader, "instance", instanceID)
}
