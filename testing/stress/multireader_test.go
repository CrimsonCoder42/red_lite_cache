// Covers: several readers on ONE sheet writing concurrently (advo_cache's unlocked server writes and
// the uncaptured reader panic, audit 2026-11-10). Its own package: it swaps the process logger.
package stress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// panicLog receives the server's and every reader's log and keeps each line that reports a
// recovered panic. The harness already fails on the counters (Stats().Panics on the server and on
// each reader, checked at cleanup); the log also catches a panic on a connection no counter covers
// (the replica stream).
type panicLog struct {
	mu    sync.Mutex
	lines []string
	// kicks counts the server's slow-consumer kicks (D-014); drops counts reader disconnects by
	// cause, so a run shows why a never-cut reader was not connected.
	kicks int
	drops map[string]int
}

func (p *panicLog) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case bytes.Contains(b, []byte("panic recovered")):
		p.lines = append(p.lines, string(bytes.TrimSpace(b)))
	case bytes.Contains(b, []byte("collaborator kicked")):
		p.kicks++
	case bytes.Contains(b, []byte("Reader: disconnected")):
		cause := "?"
		if i := bytes.Index(b, []byte("err=")); i >= 0 {
			cause = string(bytes.TrimSpace(b[i+4:]))
			if len(cause) > 90 {
				cause = cause[:90]
			}
		}
		if p.drops == nil {
			p.drops = map[string]int{}
		}
		p.drops[cause]++
	}
	return len(b), nil
}

func (p *panicLog) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("server slow-consumer kicks %d; reader disconnects by cause %v", p.kicks, p.drops)
}

func (p *panicLog) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...)
}

// TestMultiReaderConcurrentWrites puts 6 readers on one sheet (2 behind proxies that are cut over
// and over) with 4 goroutines each doing random sets, deletes, prefix and suffix deletes and gets,
// plus a goroutine that opens and closes a seventh reader in a loop (join/leave during
// broadcasts). Every server broadcast fans out to 5+ connections while their own replies are
// queued. It fails on any recovered panic (server or reader), on any delete the server refused,
// and unless, once quiet, every reader's copy equals the server's.
func TestMultiReaderConcurrentWrites(t *testing.T) {
	const (
		nDirect  = 4
		nProxied = 2
		perRdr   = 4
		nKeys    = 32
		duration = 2 * time.Second
	)
	plog := &panicLog{}
	prev := logger.L
	logger.L = logger.New(plog, slog.LevelInfo, false) // before any server goroutine reads it
	t.Cleanup(func() { logger.L = prev })              // runs last: after the server has stopped
	rlog := func(c *reader.Config) {
		c.Logger = slog.New(slog.NewTextHandler(plog, nil))
		c.MissWindow = 20 * time.Millisecond // as raceStress: read-throughs between sweeps still store
	}

	srv := integration.StartLeader(t)
	name := integration.UniqueSheet(t)
	var readers []*reader.Reader
	for range nDirect {
		readers = append(readers, integration.NewReader(t, srv, name, rlog))
	}
	var proxies []*integration.Proxy
	for range nProxied {
		p := integration.NewProxy(t, srv.Addr)
		proxies = append(proxies, p)
		readers = append(readers, integration.NewReaderAt(t, p.Addr, "", name, rlog))
	}
	keys := make([]string, nKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("k:%d:%d", i%4, i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var sets, stale, notConn, timeouts, directDown, deletes, gets, cuts, churns atomic.Int64
	var seq atomic.Int64
	errs := make(chan error, len(readers)*perRdr+2)
	var wg sync.WaitGroup
	for ri, r := range readers {
		direct := ri < nDirect
		for range perRdr {
			wg.Go(func() {
				for ctx.Err() == nil {
					k := keys[rand.IntN(nKeys)]
					var err error
					switch rand.IntN(6) {
					case 0, 1:
						err = r.SetWithTTL(context.Background(), k, seq.Add(1), 5)
						if errors.Is(err, reader.ErrStale) {
							stale.Add(1)
							err = nil
						} else if errors.Is(err, reader.ErrNotConnected) || errors.Is(err, reader.ErrTimeout) {
							if errors.Is(err, reader.ErrTimeout) {
								timeouts.Add(1)
							} else {
								notConn.Add(1)
							}
							if direct {
								directDown.Add(1) // never cut: only a server kick or a timeout puts it here
							}
							err = nil
						} else if err == nil {
							sets.Add(1)
						}
					case 2:
						err = r.Delete(context.Background(), k)
						deletes.Add(1)
					case 3:
						err = r.DeletePrefix(context.Background(), fmt.Sprintf("k:%d:", rand.IntN(4)))
						deletes.Add(1)
					case 4:
						err = r.DeleteSuffix(context.Background(), fmt.Sprintf(":%d", rand.IntN(nKeys)))
						deletes.Add(1)
					default:
						var v int64
						r.Get(k, &v)
						gets.Add(1)
					}
					if err != nil {
						errs <- fmt.Errorf("%s: %w", k, err)
						return
					}
				}
			})
		}
	}
	wg.Go(func() { // cut a proxied reader's link every 150ms: teardown + replay under load
		for i := 0; ctx.Err() == nil; i++ {
			time.Sleep(150 * time.Millisecond)
			proxies[i%len(proxies)].Cut()
			cuts.Add(1)
		}
	})
	wg.Go(func() { // join and leave the sheet while it broadcasts
		for ctx.Err() == nil {
			r, err := reader.New(reader.Config{LeaderAddr: srv.ClientAddr, Sheet: name, Password: "pw",
				ConnectTimeout: 2 * time.Second, Logger: slog.New(slog.NewTextHandler(plog, nil))})
			if err != nil {
				errs <- fmt.Errorf("churn reader: %w", err)
				return
			}
			_ = r.Set(context.Background(), keys[rand.IntN(nKeys)], seq.Add(1))
			r.Close()
			if n := r.Stats().Panics; n > 0 {
				errs <- fmt.Errorf("churn reader recovered %d panics", n)
				return
			}
			churns.Add(1)
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	t.Logf("readers %d on one sheet: %d sets stored, %d refused stale, %d not connected, %d timeouts (%d of them on never-cut readers), %d deletes, %d gets, %d proxy cuts, %d churn opens",
		len(readers), sets.Load(), stale.Load(), notConn.Load(), timeouts.Load(), directDown.Load(), deletes.Load(), gets.Load(), cuts.Load(), churns.Load())
	t.Log(plog.summary())
	if sets.Load() == 0 || deletes.Load() == 0 || cuts.Load() == 0 || churns.Load() == 0 {
		t.Fatal("load not exercised")
	}

	// Quiet: every reader reconnects and its copy converges to the server's exactly.
	for i, r := range readers {
		integration.Eventually(t, fmt.Sprintf("reader %d connected", i), r.IsConnected)
	}
	_, st := integration.OpenRaw(t, srv, name)
	for i, r := range readers {
		integration.Eventually(t, fmt.Sprintf("reader %d equals the server (%d keys)", i, len(st.Data)), func() bool {
			for _, k := range keys {
				var v int64
				ok := r.Get(k, &v)
				want, onServer := st.Data[k]
				if ok != onServer {
					return false
				}
				if ok && fmt.Sprint(v) != string(want) {
					return false
				}
			}
			return true
		})
	}

	if p := plog.got(); len(p) > 0 {
		t.Fatalf("%d recovered panics:\n%s", len(p), p)
	}
}
