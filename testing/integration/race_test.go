// Covers: reader R6 (set base_seq, stale_write against the sweep log)
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// TestRaceLoop is the RACE LOOP (R6): a read-through that missed before a write+sweep cannot cache
// the value it read, for each sweep shape; under concurrent load no stale value survives; and a
// read-through with no write in between still caches.
func TestRaceLoop(t *testing.T) {
	sweeps := []struct {
		name  string
		key   string
		sweep func(*reader.Reader) error
	}{
		{"prefix", "user:1:profile", func(r *reader.Reader) error { return r.DeletePrefix(context.Background(), "user:1:") }},
		{"exact", "user:2:profile", func(r *reader.Reader) error { return r.Delete(context.Background(), "user:2:profile") }},
		{"suffix", "user:3:list", func(r *reader.Reader) error { return r.DeleteSuffix(context.Background(), ":list") }},
	}
	for _, sw := range sweeps {
		t.Run(sw.name, func(t *testing.T) {
			srv := StartLeader(t)
			name := UniqueSheet(t)
			a := NewReader(t, srv, name)
			b := NewReader(t, srv, name)
			database := "OLD"

			var s string
			if a.Get(sw.key, &s) {
				t.Fatal("A hit a key nobody set")
			}
			readThrough := database // A reads the database after its miss

			database = "NEW" // B writes the database, then sweeps
			if err := sw.sweep(b); err != nil {
				t.Fatalf("sweep: %v", err)
			}

			err := a.SetWithTTL(context.Background(), sw.key, readThrough, 5)
			if !errors.Is(err, reader.ErrStale) {
				t.Fatalf("read-through set after the sweep = %v, want ErrStale", err)
			}
			if a.Get(sw.key, &s) || b.Get(sw.key, &s) {
				t.Fatalf("a reader holds %q after a refused set", s)
			}
			raw, _ := OpenRaw(t, srv, name)
			serverMisses(t, raw, sw.key)
		})
	}

	t.Run("no_race_caches", func(t *testing.T) {
		srv := StartLeader(t)
		name := UniqueSheet(t)
		a := NewReader(t, srv, name)
		b := NewReader(t, srv, name)
		const key = "user:9:profile"
		var s string
		if a.Get(key, &s) {
			t.Fatal("A hit a key nobody set")
		}
		if err := a.SetWithTTL(context.Background(), key, "NEW", 5); err != nil {
			t.Fatalf("read-through set with no write in between: %v", err)
		}
		Eventually(t, "B sees the read-through value", func() bool { return b.Get(key, &s) && s == "NEW" })
		raw, _ := OpenRaw(t, srv, name)
		if g := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: key}); !g.Success || string(g.Value) != `"NEW"` {
			t.Fatalf("server get = %+v", g)
		}
	})

	// Two read-throughs of one key on ONE reader (backendAdvo: one reader, many request goroutines):
	// the first misses and reads OLD; a write+sweep lands and reaches the reader; the second misses
	// (now after the sweep). The first's set must still be refused: a later miss of the same key
	// must not raise the base_seq an earlier read-through sends.
	t.Run("two_misses_one_reader", func(t *testing.T) {
		srv := StartLeader(t)
		name := UniqueSheet(t)
		a := NewReader(t, srv, name)
		b := NewReader(t, srv, name)
		const key = "user:5:profile"
		ctx := context.Background()
		var s string
		if a.Get(key, &s) { // read-through 1 misses
			t.Fatal("A hit a key nobody set")
		}
		first := "OLD"                                         // read-through 1 reads the database
		if err := b.DeletePrefix(ctx, "user:5:"); err != nil { // database → NEW, sweep
			t.Fatalf("sweep: %v", err)
		}
		if err := b.Set(ctx, "marker", 1); err != nil {
			t.Fatalf("marker: %v", err)
		}
		Eventually(t, "A applied the sweep (marker after it)", func() bool { return a.Has("marker") })
		if a.Get(key, &s) { // read-through 2 misses, after the sweep
			t.Fatal("A hit the swept key")
		}
		if err := a.SetWithTTL(ctx, key, first, 5); !errors.Is(err, reader.ErrStale) {
			t.Fatalf("read-through 1's set (read before the sweep) = %v, want ErrStale", err)
		}
		raw, _ := OpenRaw(t, srv, name)
		serverMisses(t, raw, key)
	})

	t.Run("stress", raceStress)
}

// raceStress runs N read-through goroutines (miss → read the database → jitter → set), several per
// reader on the same keys (the consumer shape: one reader, many request goroutines), against M
// writer goroutines (bump the database → sweep) on one more reader. When everything has stopped,
// every key's value — on the server and on every reader — is absent or the database's current
// version: a version a writer superseded and swept never survives. MissWindow is 20ms (read-through
// jitter is at most 2ms) so read-throughs between writes still get stored.
func raceStress(t *testing.T) {
	const (
		nKeys    = 8
		nReaders = 3
		perRdr   = 4
		nWriters = 3
		duration = 1500 * time.Millisecond
	)
	srv := StartLeader(t)
	name := UniqueSheet(t)
	window := func(c *reader.Config) { c.MissWindow = 20 * time.Millisecond }
	readers := make([]*reader.Reader, nReaders)
	for i := range readers {
		readers[i] = NewReader(t, srv, name, window)
	}
	w := NewReader(t, srv, name)
	keys := make([]string, nKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("s:%d:v", i)
	}

	var dbMu sync.Mutex
	database := make(map[string]int, nKeys) // the "database": a version per key
	read := func(k string) int { dbMu.Lock(); defer dbMu.Unlock(); return database[k] }

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var stale, stored atomic.Int64
	errs := make(chan error, nReaders*perRdr+nWriters)
	var wg sync.WaitGroup
	for i := range nReaders * perRdr {
		r := readers[i%nReaders]
		wg.Go(func() {
			for ctx.Err() == nil {
				k := keys[rand.IntN(nKeys)]
				var v int
				if r.Get(k, &v) {
					continue
				}
				v = read(k)
				time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
				switch err := r.SetWithTTL(context.Background(), k, v, 5); {
				case err == nil:
					stored.Add(1)
				case errors.Is(err, reader.ErrStale):
					stale.Add(1)
				default:
					errs <- fmt.Errorf("read-through set %s: %w", k, err)
					return
				}
			}
		})
	}
	for i := range nWriters {
		wg.Go(func() {
			for n := 0; ctx.Err() == nil; n++ {
				k := keys[rand.IntN(nKeys)]
				var err error
				dbMu.Lock()
				switch (n + i) % 3 {
				case 0: // exact: bump one key
					database[k]++
				default: // prefix / suffix: bump every key
					for _, kk := range keys {
						database[kk]++
					}
				}
				dbMu.Unlock()
				switch (n + i) % 3 {
				case 0:
					err = w.Delete(context.Background(), k)
				case 1:
					err = w.DeletePrefix(context.Background(), "s:")
				default:
					err = w.DeleteSuffix(context.Background(), ":v")
				}
				if err != nil {
					errs <- fmt.Errorf("sweep: %w", err)
					return
				}
				time.Sleep(time.Duration(rand.IntN(10)) * time.Millisecond)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	t.Logf("read-through sets: %d stored, %d refused stale", stored.Load(), stale.Load())
	if stale.Load() == 0 || stored.Load() == 0 {
		t.Fatalf("race not exercised: %d stored, %d stale; want both > 0", stored.Load(), stale.Load())
	}

	// Server: absent or current.
	raw, _ := OpenRaw(t, srv, name)
	for _, k := range keys {
		g := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: k})
		if !g.Success {
			if g.Error != protocol.ErrMsgKeyNotFound {
				t.Fatalf("server get %s = %+v", k, g)
			}
			continue
		}
		var v int
		if err := json.Unmarshal(g.Value, &v); err != nil || v != read(k) {
			t.Fatalf("server holds %s = %s, database is at %d (stale value survived a sweep)", k, g.Value, read(k))
		}
	}
	// Readers: converge to the server, so absent or current.
	for i, r := range append(readers, w) {
		Eventually(t, fmt.Sprintf("reader %d holds no stale version", i), func() bool {
			for _, k := range keys {
				var v int
				if r.Get(k, &v) && v != read(k) {
					return false
				}
			}
			return true
		})
	}

	// A final write + sweep with no read-through after it: nothing is cached anywhere.
	if err := w.DeletePrefix(context.Background(), "s:"); err != nil {
		t.Fatalf("final sweep: %v", err)
	}
	serverMisses(t, raw, keys...)
	for i, r := range append(readers, w) {
		Eventually(t, fmt.Sprintf("reader %d empty after the final sweep", i), func() bool {
			for _, k := range keys {
				if r.Has(k) {
					return false
				}
			}
			return true
		})
	}
}
