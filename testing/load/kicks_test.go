// Covers: slow-consumer kicks under a fixed write rate (S4, doc/reader_optimize_2026-11-10). Its own
// package: it swaps the process logger to count the server's kick and join lines. Measures; asserts
// only that the load ran.
package load

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// lineCounter counts the server's "Sheet: collaborator kicked" (D-014 slow consumer) and
// "Sheet: joined" (every open, so every resent sheet_state) lines.
type lineCounter struct {
	kicks, joins atomic.Int64
}

func (c *lineCounter) Write(b []byte) (int, error) {
	switch {
	case bytes.Contains(b, []byte("Sheet: collaborator kicked")):
		c.kicks.Add(1)
	case bytes.Contains(b, []byte("Sheet: joined")):
		c.joins.Add(1)
	}
	return len(b), nil
}

// cell is one measurement: a write rate held for duration against one send-queue size.
type cell struct {
	rate, queue                        int
	sent, missed                       int64 // Sets started; tokens the pacer could not hand out
	kicks, resent                      int64
	notConn, timeouts, stale, otherErr int64
}

// TestLoadKicks (S4): 3 readers on one sheet; together they hold a fixed rate of Sets (each reader
// a third) for 3 s, for every rate × REDLITE_CONN_SEND_QUEUE pair. Every Set is broadcast to the
// other two readers, so each reader receives about the whole rate. Reports per cell: kicks, resent
// sheet_state frames (joins after the first 3), and Set errors. Runs only with
// REDLITE_LOAD_TEST=1 (about 40 s); the harness runs it with -race, so absolute rates are lower
// than a production build's.
func TestLoadKicks(t *testing.T) {
	if os.Getenv("REDLITE_LOAD_TEST") != "1" {
		t.Skip("SKIPPED: load (set REDLITE_LOAD_TEST=1)")
	}
	const (
		readers  = 3
		workers  = 16 // per reader: Sets wait for their ack, so this bounds the in-flight count
		duration = 3 * time.Second
	)
	lines := &lineCounter{}
	prev := logger.L
	logger.L = logger.New(lines, slog.LevelInfo, false) // before any server goroutine reads it
	t.Cleanup(func() { logger.L = prev })               // runs last: after every server stopped
	quiet := func(c *reader.Config) { c.Logger = slog.New(slog.NewTextHandler(discard{}, nil)) }

	// The last pair is the control: an 8-frame queue at 5 000/s must show kicks, or a row of zeros
	// above would only mean the counters see nothing.
	type pair struct{ queue, rate int }
	var plan []pair
	for _, queue := range []int{256, 1024, 4096} {
		for _, rate := range []int{50, 500, 5000} {
			plan = append(plan, pair{queue, rate})
		}
	}
	plan = append(plan, pair{8, 5000})
	var cells []cell
	for _, p := range plan {
		queue, rate := p.queue, p.rate
		{
			c := cell{rate: rate, queue: queue}
			srv := integration.StartLeader(t, func(cfg *config.Config) { cfg.ConnSendQueue = queue })
			name := integration.UniqueSheet(t)
			rs := make([]*reader.Reader, readers)
			for i := range rs {
				rs[i] = integration.NewReader(t, srv, name, quiet)
			}
			kick0, join0 := lines.kicks.Load(), lines.joins.Load()

			ctx, cancel := context.WithTimeout(context.Background(), duration)
			var sent, missed, notConn, timeouts, stale, other atomic.Int64
			var wg sync.WaitGroup
			for ri, r := range rs {
				tokens := make(chan struct{}, rate)
				wg.Go(func() { // pacer: rate/readers tokens per second, handed out every 10 ms
					defer close(tokens)
					per := float64(rate) / readers
					tk := time.NewTicker(10 * time.Millisecond)
					defer tk.Stop()
					start, given := time.Now(), 0
					for {
						select {
						case <-ctx.Done():
							return
						case now := <-tk.C:
							due := int(per * now.Sub(start).Seconds())
							for ; given < due; given++ {
								select {
								case tokens <- struct{}{}:
								default:
									missed.Add(1)
								}
							}
						}
					}
				})
				for w := range workers {
					wg.Go(func() {
						n := 0
						for range tokens {
							n++
							sent.Add(1)
							key := fmt.Sprintf("load:%d:%d:%d", ri, w, n%64)
							switch err := r.Set(context.Background(), key, n); {
							case err == nil:
							case errors.Is(err, reader.ErrNotConnected):
								notConn.Add(1)
							case errors.Is(err, reader.ErrTimeout):
								timeouts.Add(1)
							case errors.Is(err, reader.ErrStale):
								stale.Add(1)
							default:
								other.Add(1)
							}
						}
					})
				}
			}
			wg.Wait()
			cancel()
			c.sent, c.missed = sent.Load(), missed.Load()
			c.notConn, c.timeouts, c.stale, c.otherErr = notConn.Load(), timeouts.Load(), stale.Load(), other.Load()
			c.kicks = lines.kicks.Load() - kick0
			c.resent = lines.joins.Load() - join0
			t.Logf("LOAD queue=%d rate=%d/s: sent %d (%.0f/s) missed %d | kicks %d | resent sheet_state %d | Set errors: not connected %d, timeout %d, stale %d, other %d",
				c.queue, c.rate, c.sent, float64(c.sent)/duration.Seconds(), c.missed, c.kicks, c.resent, c.notConn, c.timeouts, c.stale, c.otherErr)
			cells = append(cells, c)
			srv.Stop()
		}
	}
	for _, c := range cells {
		if c.sent == 0 {
			t.Fatalf("queue=%d rate=%d: no Set sent", c.queue, c.rate)
		}
	}
	if ctl := cells[len(cells)-1]; ctl.kicks == 0 || ctl.resent == 0 {
		t.Fatalf("control (queue 8, 5 000/s): kicks %d, resent %d — the counters see nothing", ctl.kicks, ctl.resent)
	}
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }
