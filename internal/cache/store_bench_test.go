// Covers: internal/cache
package cache

import (
	"fmt"
	"strings"
	"testing"
)

const (
	benchKeys  = 100_000
	benchRoots = 50
)

// benchKey is the consumer's key grammar: <root>:patient:<id>:<view>.
func benchKey(i int) string {
	return fmt.Sprintf("root%d:patient:%d:view%d", i%benchRoots, i, i%4)
}

// names is every bench key, built once so Get/Set benchmarks do not time fmt.Sprintf.
var names = func() []string {
	out := make([]string, benchKeys)
	for i := range out {
		out[i] = benchKey(i)
	}
	return out
}()

func benchStore() *Store {
	s := New()
	for i := range benchKeys {
		s.Set(benchKey(i), Entry{Value: val("v")})
	}
	return s
}

// naiveDeletePrefix is the v1 shape (advo_cache/cache/cache.go:62-74): scan every key.
func naiveDeletePrefix(s *Store, prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var victims []string
	for k := range s.m {
		if strings.HasPrefix(k, prefix) {
			victims = append(victims, k)
		}
	}
	for _, k := range victims {
		s.remove(k)
	}
	return len(victims)
}

func BenchmarkStoreGetParallel(b *testing.B) {
	s := benchStore()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s.Get(names[i%benchKeys], t0)
			i += 7919
		}
	})
}

func BenchmarkStoreSet(b *testing.B) {
	s := benchStore()
	e := Entry{Value: val("v")}
	i := 0
	for b.Loop() {
		s.Set(names[i%benchKeys], e)
		i++
	}
}

// One root (2 000 of 100 000 keys) is swept, then restored outside the timer.
func benchDeletePrefix(b *testing.B, del func(*Store, string) int) {
	s := benchStore()
	const prefix = "root7:"
	var swept []string
	for k := range s.All(t0) {
		if strings.HasPrefix(k, prefix) {
			swept = append(swept, k)
		}
	}
	e := Entry{Value: val("v")}
	for b.Loop() {
		if n := del(s, prefix); n != len(swept) {
			b.Fatalf("deleted %d, want %d", n, len(swept))
		}
		b.StopTimer()
		for _, k := range swept {
			s.Set(k, e)
		}
		b.StartTimer()
	}
}

func BenchmarkDeletePrefixIndexed(b *testing.B) {
	benchDeletePrefix(b, (*Store).DeletePrefix)
}

func BenchmarkDeletePrefixNaive(b *testing.B) {
	benchDeletePrefix(b, naiveDeletePrefix)
}

// A sweep that matches nothing inside a populated root: the pure scan cost.
func BenchmarkDeletePrefixMissIndexed(b *testing.B) {
	s := benchStore()
	for b.Loop() {
		s.DeletePrefix("root7:patient:nope:")
	}
}

func BenchmarkDeletePrefixMissNaive(b *testing.B) {
	s := benchStore()
	for b.Loop() {
		naiveDeletePrefix(s, "root7:patient:nope:")
	}
}
