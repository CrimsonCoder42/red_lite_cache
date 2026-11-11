// Covers: the docker compose stack (doc/docker_ready_2026-11-11)

// Package compose checks a running `docker compose up` stack from the outside, over the wire, with
// the public reader: the bootstrap's key file admits a reader that sets, reads and clears a key,
// and — when asked to restart the leader — reconnects by itself and finds a key restored from
// PostgreSQL. Gated: it runs only with REDLITE_COMPOSE_TEST=1 against a stack that is already up
// (testing/compose_smoke.sh starts one; `make up` for the developer's own).
//
//	REDLITE_COMPOSE_TEST=1          required
//	REDLITE_COMPOSE_KEYFILE         default ../../.redlite/dev-project.env
//	REDLITE_COMPOSE_RESTART=1       also restart the leader (docker compose restart leader)
//	REDLITE_COMPOSE_PROJECT         the compose project name for that restart (default: compose's own)
//	REDLITE_COMPOSE_ENV_FILE        the env file for that restart (compose_smoke.sh passes its own)
package compose

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// keyFile reads the bootstrap's KEY=VALUE file.
func keyFile(t *testing.T) map[string]string {
	t.Helper()
	path := os.Getenv("REDLITE_COMPOSE_KEYFILE")
	if path == "" {
		path = filepath.Join("..", "..", ".redlite", "dev-project.env")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("bootstrap key file: %v (is the stack up? `make up`)", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	for _, k := range []string{"REDLITE_DEV_ADDR", "REDLITE_DEV_API_KEY", "REDLITE_DEV_SHEET", "REDLITE_DEV_SHEET_PASSWORD"} {
		if out[k] == "" {
			t.Fatalf("%s: %s missing", path, k)
		}
	}
	return out
}

// reconnectCounter counts the reader's "Reader: reconnected" log lines and drops the rest.
type reconnectCounter struct{ n *atomic.Int64 }

func (h reconnectCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h reconnectCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "Reader: reconnected" {
		h.n.Add(1)
	}
	return nil
}
func (h reconnectCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h reconnectCounter) WithGroup(string) slog.Handler      { return h }

// waitUntil polls cond every 50 ms until it holds or d passes.
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestComposeReaderRoundTrip(t *testing.T) {
	if os.Getenv("REDLITE_COMPOSE_TEST") != "1" {
		t.Skip("SKIPPED: compose (set REDLITE_COMPOSE_TEST=1 with the stack up)")
	}
	kf := keyFile(t)
	var reconnects atomic.Int64
	r, err := reader.New(reader.Config{
		LeaderAddr: kf["REDLITE_DEV_ADDR"], Sheet: kf["REDLITE_DEV_SHEET"], Password: kf["REDLITE_DEV_SHEET_PASSWORD"],
		APIKey: kf["REDLITE_DEV_API_KEY"], ConnectTimeout: 10 * time.Second,
		ReconnectMin: 50 * time.Millisecond, ReconnectMax: time.Second,
		Logger: slog.New(reconnectCounter{&reconnects}),
	})
	if err != nil {
		t.Fatalf("sign in with the bootstrap key: %v", err)
	}
	defer r.Close()
	waitUntil(t, 10*time.Second, "connected", r.IsConnected)

	ctx := context.Background()
	prefix := fmt.Sprintf("compose-check:%d:", time.Now().UnixNano())
	gone, kept := prefix+"cleared", prefix+"kept"
	defer r.DeletePrefix(context.Background(), prefix) //nolint:errcheck // cleanup of this run's keys

	// set, read, clear
	if err := r.Set(ctx, gone, "v1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	var got string
	if !r.Get(gone, &got) || got != "v1" {
		t.Fatalf("get after set: %q", got)
	}
	if err := r.DeletePrefix(ctx, gone); err != nil {
		t.Fatalf("delete_prefix: %v", err)
	}
	if r.Has(gone) {
		t.Fatal("key still present after its prefix was cleared")
	}
	if err := r.Set(ctx, kept, "survives"); err != nil {
		t.Fatalf("set: %v", err)
	}

	if os.Getenv("REDLITE_COMPOSE_RESTART") != "1" {
		return
	}
	// Restart the leader: the reader must reconnect by itself, and the key must come back from
	// PostgreSQL (persistence on; the leader flushes on SIGTERM and hydrates at boot).
	args := []string{"compose"}
	if p := os.Getenv("REDLITE_COMPOSE_PROJECT"); p != "" {
		args = append(args, "-p", p)
	}
	if f := os.Getenv("REDLITE_COMPOSE_ENV_FILE"); f != "" { // the smoke's own env file: never the developer's .env
		args = append(args, "--env-file", f)
	}
	args = append(args, "restart", "leader")
	cmd := exec.Command("docker", args...)
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	// "Reader: reconnected" is logged after the new sheet_state replaced the local map, so a Get
	// after it reads what the restarted leader holds, never the copy from before the restart.
	waitUntil(t, 60*time.Second, "the reader reconnected after the restart", func() bool {
		return reconnects.Load() > 0 && r.IsConnected()
	})
	waitUntil(t, 10*time.Second, "the key restored from PostgreSQL", func() bool {
		var v string
		return r.Get(kept, &v) && v == "survives"
	})
}
