// Covers: cmd/redlite (production boot without REDLITE_CLUSTER_TOKEN — D-031, tenancy criterion 10)
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// run refuses a production config with an empty cluster token before anything starts.
func TestRunRefusesProductionWithoutClusterToken(t *testing.T) {
	t.Chdir(t.TempDir()) // config.Load reads ./.env: none here
	t.Setenv("REDLITE_ENVIRONMENT", "production")
	t.Setenv("REDLITE_CLUSTER_TOKEN", "")
	t.Setenv("REDLITE_PORT", "127.0.0.1:0") // never a fixed port, even if run got that far
	err := run()
	if err == nil || !strings.Contains(err.Error(), "REDLITE_CLUSTER_TOKEN") {
		t.Fatalf("run() = %v; want an error naming REDLITE_CLUSTER_TOKEN", err)
	}
}

// The built binary exits 1 with exactly one Error line naming the variable.
func TestBinaryExitsOneWithOneErrorLine(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("SKIPPED: binary exit test (go not on PATH)")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "redlite")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = dir // no .env
	env := []string{"REDLITE_ENVIRONMENT=production", "REDLITE_CLUSTER_TOKEN=", "REDLITE_PORT=127.0.0.1:0"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "REDLITE_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("binary still running after 5s (it started instead of refusing):\n%s", out.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v; want exit status 1\n%s", err, out.String())
	}
	var errorLines []string
	for line := range strings.Lines(out.String()) {
		if strings.Contains(line, "level=ERROR") {
			errorLines = append(errorLines, line)
		}
	}
	if len(errorLines) != 1 || !strings.Contains(errorLines[0], "REDLITE_CLUSTER_TOKEN") {
		t.Fatalf("want exactly one level=ERROR line naming REDLITE_CLUSTER_TOKEN, got %d:\n%s", len(errorLines), out.String())
	}
}
