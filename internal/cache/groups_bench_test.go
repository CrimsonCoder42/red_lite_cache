package cache

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The S9 benchmark targets (doc/specs/s9_ordered_structures.md § Benchmark targets) were measured
// on a prototype store: a map, a dense slot array for uniform eviction draws (D-043's pattern) and
// the two ordered indexes. gstore is that store around the real Groups, so these numbers compare
// with the targets one to one. The two key sets are groups_test.go's colon and path sets.

type gentry struct {
	val []byte
	pos int
}

type gstore struct {
	mu    sync.RWMutex
	_     [128]byte
	m     map[string]gentry
	slots []string
	g     *Groups
	max   int
	buf   []string
	rng   *rand.Rand
}

func newGStore(g *Groups, max int) *gstore {
	return &gstore{m: map[string]gentry{}, g: g, max: max, rng: rand.New(rand.NewPCG(3, 4))}
}

var gval = []byte(`{"id":1}`)

func (s *gstore) get(k string) bool {
	s.mu.RLock()
	_, ok := s.m[k]
	s.mu.RUnlock()
	return ok
}

func (s *gstore) dropLocked(k string) {
	e := s.m[k]
	last := len(s.slots) - 1
	if e.pos != last {
		moved := s.slots[last]
		s.slots[e.pos] = moved
		me := s.m[moved]
		me.pos = e.pos
		s.m[moved] = me
	}
	s.slots[last] = ""
	s.slots = s.slots[:last]
	delete(s.m, k)
}

func (s *gstore) set(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[k]; ok {
		return
	}
	if s.max > 0 && len(s.m) >= s.max {
		v := s.slots[s.rng.IntN(len(s.slots))]
		s.dropLocked(v)
		s.g.Remove(v)
	}
	s.m[k] = gentry{val: gval, pos: len(s.slots)}
	s.slots = append(s.slots, k)
	s.g.Add(k)
}

func (s *gstore) deletePrefix(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = s.g.CutPrefix(s.buf[:0], p)
	for _, k := range s.buf {
		s.dropLocked(k)
	}
	return len(s.buf)
}

func (s *gstore) deleteSuffix(suf string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = s.g.CutSuffix(s.buf[:0], suf)
	for _, k := range s.buf {
		s.dropLocked(k)
	}
	return len(s.buf)
}

func (s *gstore) scan(from string, n int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.g.Scan(make([]string, 0, n), from, n)
}

func filledG(g *Groups, ks keySetT, max int) *gstore {
	s := newGStore(g, max)
	for _, k := range ks.keys {
		s.set(k)
	}
	return s
}

var gsizes = []int{1_000, 10_000, 100_000}

func eachG(b *testing.B, fn func(b *testing.B, ks keySetT, n int)) {
	for _, name := range genericNames {
		for _, n := range gsizes {
			ks := genericSets[name](n)
			b.Run(fmt.Sprintf("keys=%s/n=%d", name, n), func(b *testing.B) { fn(b, ks, n) })
		}
	}
}

func BenchmarkGroupsGet(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) {
		s := filledG(NewGroups(), ks, 0)
		i := 0
		for b.Loop() {
			if !s.get(ks.keys[(i*7919)%n]) {
				b.Fatal("miss")
			}
			i++
		}
	})
}

func BenchmarkGroupsGetParallel(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) {
		s := filledG(NewGroups(), ks, 0)
		var seed atomic.Int64
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := int(seed.Add(7919))
			for pb.Next() {
				if !s.get(ks.keys[i%n]) {
					b.Fatal("miss")
				}
				i++
			}
		})
	})
}

// freshKeys are new keys for SetFull, built before the timer.
func freshKeys(ks keySetT, n int) []string {
	out := make([]string, 1<<16)
	for i := range out {
		out[i] = ks.keys[i%n] + "~" + fmt.Sprint(i)
	}
	return out
}

// BenchmarkGroupsSetFull: a new key on a map at max: one eviction (index remove) + one insert.
func BenchmarkGroupsSetFull(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) { benchSetFull(b, NewGroups(), ks, n) })
}

func benchSetFull(b *testing.B, g *Groups, ks keySetT, n int) {
	s := filledG(g, ks, n)
	fresh := freshKeys(ks, n)
	i := 0
	for b.Loop() {
		s.set(fresh[i&(1<<16-1)] + fmt.Sprint(i>>16))
		i++
	}
}

// BenchmarkGroupsDeletePrefix1: a prefix matching ONE key (the whole key), then the key is put back.
func BenchmarkGroupsDeletePrefix1(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) { benchPrefix1(b, NewGroups(), ks, n) })
}

func benchPrefix1(b *testing.B, g *Groups, ks keySetT, n int) {
	s := filledG(g, ks, 0)
	i := 0
	for b.Loop() {
		k := ks.keys[(i*7919)%n]
		if s.deletePrefix(k) != 1 {
			b.Fatalf("%q: not one match", k)
		}
		s.set(k)
		i++
	}
}

// BenchmarkGroupsDeleteSuffix1: a suffix matching ONE key, then the key is put back.
func BenchmarkGroupsDeleteSuffix1(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) {
		s := filledG(NewGroups(), ks, 0)
		var idx []int
		for i, suf := range ks.suffix {
			if suf != "" {
				idx = append(idx, i)
			}
		}
		i := 0
		for b.Loop() {
			j := idx[(i*7919)%len(idx)]
			if got := s.deleteSuffix(ks.suffix[j]); got != 1 {
				b.Fatalf("%q: %d matches", ks.suffix[j], got)
			}
			s.set(ks.keys[j])
			i++
		}
	})
}

// BenchmarkGroupsScan100: 100 keys in order from a present key.
func BenchmarkGroupsScan100(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) { benchScan(b, NewGroups(), ks, n) })
}

func benchScan(b *testing.B, g *Groups, ks keySetT, n int) {
	s := filledG(g, ks, 0)
	i := 0
	for b.Loop() {
		if got := s.scan(ks.keys[(i*7919)%n], 100); len(got) == 0 {
			b.Fatal("empty")
		}
		i++
	}
}

func heapInUse() uint64 {
	var ms runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// BenchmarkGroupsMemory: heap per entry for map + slots + both trees (reversed keys included); the
// keys themselves are allocated before the baseline.
func BenchmarkGroupsMemory(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) { benchMemory(b, func() *Groups { return NewGroups() }, ks, n) })
}

func benchMemory(b *testing.B, mk func() *Groups, ks keySetT, n int) {
	for b.Loop() {
		h0 := heapInUse()
		s := filledG(mk(), ks, 0)
		h1 := heapInUse()
		b.ReportMetric(float64(h1-h0)/float64(n), "B/entry")
		runtime.KeepAlive(s)
	}
}

// BenchmarkGroupsBuildSorted: the store built from a sorted snapshot (map + slots + Load), the
// shape of a reader's connect or a replica snapshot.
func BenchmarkGroupsBuildSorted(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) {
		sorted := slices.Sorted(slices.Values(ks.keys))
		for b.Loop() {
			s := newGStore(NewGroups(), 0)
			s.m = make(map[string]gentry, len(sorted))
			s.slots = append(make([]string, 0, len(sorted)), sorted...)
			for i, k := range sorted {
				s.m[k] = gentry{val: gval, pos: i}
			}
			s.g.Load(sorted)
		}
	})
}

// BenchmarkGroupsMixed: 16 writers (a new key on a full map, or a one-match prefix delete + put
// back, alternating) and 16 readers (Get) at once. ns/op is per Get; writes/s from the writers.
func BenchmarkGroupsMixed(b *testing.B) {
	eachG(b, func(b *testing.B, ks keySetT, n int) {
		s := filledG(NewGroups(), ks, n)
		stop := make(chan struct{})
		var writes atomic.Int64
		var ww sync.WaitGroup
		for g := range 16 {
			ww.Go(func() {
				for i := g; ; i += 16 {
					select {
					case <-stop:
						return
					default:
					}
					k := ks.keys[(i*7919)%n]
					if i&1 == 0 {
						s.set(k + "~" + fmt.Sprint(i))
					} else {
						s.deletePrefix(k)
						s.set(k)
					}
					writes.Add(1)
				}
			})
		}
		b.ResetTimer()
		t0, w0 := time.Now(), writes.Load()
		var rw sync.WaitGroup
		per := b.N/16 + 1
		for g := range 16 {
			rw.Go(func() {
				for i := range per {
					s.get(ks.keys[(g*104729+i*7919)%n])
				}
			})
		}
		rw.Wait()
		b.StopTimer()
		b.ReportMetric(float64(writes.Load()-w0)/time.Since(t0).Seconds(), "writes/s")
		close(stop)
		ww.Wait()
	})
}

// BenchmarkGroupsNodeSweep: node size and merge threshold (Devin's decision 2): SetFull, a one-match
// prefix delete, a 100-key scan and memory at 100,000 keys of both sets, for each candidate size.
// The winner becomes DefaultNodeKeys / DefaultMinKeys.
func BenchmarkGroupsNodeSweep(b *testing.B) {
	sizes := []struct{ max, min int }{{16, 4}, {32, 8}, {32, 16}, {64, 16}, {64, 32}, {128, 32}, {128, 64}}
	for _, name := range genericNames {
		ks := genericSets[name](100_000)
		for _, sz := range sizes {
			mk := func() *Groups { return newGroupsSized(sz.max, sz.min) }
			tag := fmt.Sprintf("keys=%s/node=%d/min=%d", name, sz.max, sz.min)
			b.Run("setfull/"+tag, func(b *testing.B) { benchSetFull(b, mk(), ks, 100_000) })
			b.Run("prefix1/"+tag, func(b *testing.B) { benchPrefix1(b, mk(), ks, 100_000) })
			b.Run("scan100/"+tag, func(b *testing.B) { benchScan(b, mk(), ks, 100_000) })
			b.Run("memory/"+tag, func(b *testing.B) { benchMemory(b, mk, ks, 100_000) })
		}
	}
}

// BenchmarkWheelCompact: one compaction of a 100,000-key store's wheel holding 2 × live + 1 items
// (every key overwritten once with a new expiry), spread over a day of seconds or all in one second
// (a bulk fill with one TTL). This is the longest a Set can hold the store lock (D-045).
func BenchmarkWheelCompact(b *testing.B) {
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, spread := range []int{86_400, 1} {
		b.Run(fmt.Sprintf("seconds=%d", spread), func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				w := NewWheel()
				exp := map[string]time.Time{}
				for i := range 100_000 {
					k := fmt.Sprintf("k:%06d", i)
					at := base.Add(time.Duration(i%spread) * time.Second)
					w.Add(k, at.Add(-time.Hour)) // the stale first expiry
					w.Add(k, at)
					exp[k] = at
				}
				w.Add("k:000000", exp["k:000000"]) // one more: items > 2 × live
				live := func(k string) (time.Time, bool) { at, ok := exp[k]; return at, ok }
				b.StartTimer()
				if !w.Compact(len(exp), live) {
					b.Fatal("no compaction")
				}
			}
		})
	}
}
