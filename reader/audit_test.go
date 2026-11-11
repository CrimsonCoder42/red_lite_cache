// Covers: reader R8 (one decode per hit) and the local prefix/suffix scan (audit 2026-11-10)
package reader

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// countingValue counts every decode into it and every encode of it.
type countingValue struct {
	decodes, encodes *atomic.Int64
	Name             string
}

func (c *countingValue) UnmarshalJSON(b []byte) error {
	c.decodes.Add(1)
	var v struct{ Name string }
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	c.Name = v.Name
	return nil
}

func (c countingValue) MarshalJSON() ([]byte, error) {
	c.encodes.Add(1)
	return json.Marshal(struct{ Name string }{c.Name})
}

// v1 (advo_cache/reader/reader.go:157-159 + backendAdvo/cache/store.go:166-178) decoded every
// broadcast into map[string]any, and the consumer marshalled and unmarshalled it again per hit.
// Get now decodes the stored bytes once, straight into the caller's target, and never encodes.
func TestR8GetDecodesOnce(t *testing.T) {
	f := newFakeServer(t)
	f.put("u", `{"Name":"u"}`, 0)
	r := mustNew(t, testConfig(f.addr()))
	f.broadcast(protocol.Replicate{Type: protocol.TypeReplicate, Key: "b", Value: json.RawMessage(`{"Name":"b"}`)})
	waitFor(t, "replicate", func() bool { return r.Has("b") })

	for _, k := range []string{"u", "b"} {
		var dec, enc atomic.Int64
		const hits = 10
		for range hits {
			v := countingValue{decodes: &dec, encodes: &enc}
			if !r.Get(k, &v) || v.Name != k {
				t.Fatalf("Get(%s) = %+v", k, v)
			}
		}
		t.Logf("%s: %d hits, %d decodes, %d encodes", k, hits, dec.Load(), enc.Load())
		if dec.Load() != hits || enc.Load() != 0 {
			t.Fatalf("%s: %d hits did %d decodes and %d encodes, want %d and 0", k, hits, dec.Load(), enc.Load(), hits)
		}
	}
}

// Local-map benchmarks (doc/reader_optimize_2026-11-10). Keys have the structure of a scoped
// consumer's (the corpus itself lives only in testing/conformance), with generic names: 4 of every
// 5 are `<root>:owner:<uuid>:<view>` (8 roots × 5 views per owner), 1 of 5 is `<entity>:record:<uuid>`.
// Renamed from the consumer's segment on 2026-11-11 (D-044); same structure, so numbers compare.
var (
	benchSizes = []int{1_000, 10_000, 100_000}
	benchRoots = []string{"condition", "med", "vitals", "episode", "pef", "allergy", "lab", "note"}
	benchViews = []string{"list", "count", "summary", "detail", "timeline"}
	benchRaw   = json.RawMessage(`{"id":1}`)
)

func benchUUID(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i) }

// benchKeys returns n distinct keys: owner keys at i%5 != 4, record keys at i%5 == 4.
func benchKeys(n int) []string {
	keys := make([]string, n)
	p := 0
	for i := range keys {
		if i%5 == 4 {
			keys[i] = "record:" + "record:" + benchUUID(i) // `<entity>:record:<id>`, entity "record"
			continue
		}
		root, view := benchRoots[p%len(benchRoots)], benchViews[(p/len(benchRoots))%len(benchViews)]
		keys[i] = root + ":owner:" + benchUUID(p/(len(benchRoots)*len(benchViews))) + ":" + view
		p++
	}
	return keys
}

// benchLocal is a local map holding keys, with room for max entries.
func benchLocal(keys []string, max int) *local {
	now := time.Now()
	l := newLocal(max, missSeqMax, DefaultMissWindow, now)
	l.mu.Lock()
	for _, k := range keys {
		l.putLocked(k, benchRaw, time.Time{}, now)
	}
	l.mu.Unlock()
	return l
}

// BenchmarkLocalDeletePrefix: one prefix delete that matches ONE key (a whole owner key), then the
// key is put back so the size holds. The cost should follow the matches, not the map.
func BenchmarkLocalDeletePrefix(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			keys := benchKeys(n)
			var targets []string
			for i, k := range keys {
				if i%5 != 4 {
					targets = append(targets, k)
				}
			}
			l := benchLocal(keys, n+1)
			now := time.Now()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := targets[i%len(targets)]
				l.mu.Lock()
				l.removeLocked(opDeletePrefix, k)
				l.putLocked(k, benchRaw, time.Time{}, now)
				l.mu.Unlock()
			}
		})
	}
}

// BenchmarkLocalDeleteSuffix: one suffix delete that matches ONE key (`:record:<uuid>`), then the
// key is put back.
func BenchmarkLocalDeleteSuffix(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			keys := benchKeys(n)
			var targets []string
			for i, k := range keys {
				if i%5 == 4 {
					targets = append(targets, k)
				}
			}
			l := benchLocal(keys, n+1)
			now := time.Now()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := targets[i%len(targets)]
				l.mu.Lock()
				l.removeLocked(opDeleteSuffix, k[len("record"):]) // ":record:<uuid>"
				l.putLocked(k, benchRaw, time.Time{}, now)
				l.mu.Unlock()
			}
		})
	}
}

// BenchmarkLocalGetParallel: the local half of Reader.Get (a clock read and a lookup) from every
// CPU at once, over keys that are all present.
func BenchmarkLocalGetParallel(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			keys := benchKeys(n)
			l := benchLocal(keys, n+1)
			var seed atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := int(seed.Add(7919))
				for pb.Next() {
					if _, ok := l.lookup(keys[i%n], time.Now()); !ok {
						b.Fatal("miss")
					}
					i++
				}
			})
		})
	}
}

// BenchmarkLocalPutFull: a new key on a map at MaxEntries, so every put evicts one entry.
func BenchmarkLocalPutFull(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			l := benchLocal(benchKeys(n), n)
			now := time.Now()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Every key is new, so every put evicts (the key's allocation is in the loop, as in a
				// real frame).
				k := "fresh:owner:" + strconv.Itoa(i) + ":list"
				l.mu.Lock()
				l.putLocked(k, benchRaw, time.Time{}, now)
				l.mu.Unlock()
			}
			if l.len() > n {
				b.Fatalf("%d entries > MaxEntries %d", l.len(), n)
			}
		})
	}
}

// BenchmarkLocalBytesPerEntry reports the heap one entry costs in the local map (map slot, entry and
// any index), the key strings and the value bytes excluded: they are allocated before the baseline
// and shared.
func BenchmarkLocalBytesPerEntry(b *testing.B) {
	const n = 100_000
	keys := benchKeys(n)
	var ms runtime.MemStats
	for i := 0; i < b.N; i++ {
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&ms)
		before := ms.HeapAlloc
		l := benchLocal(keys, n+1)
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&ms)
		b.ReportMetric(float64(ms.HeapAlloc-before)/n, "B/entry")
		runtime.KeepAlive(l)
	}
}
