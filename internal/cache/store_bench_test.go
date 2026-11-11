// Covers: internal/cache
package cache

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	benchKeys  = 100_000
	benchRoots = 50
)

// benchKey is a generic scoped grammar, <root>:owner:<id>:<view> (no consumer shape in internal/;
// the same structure as before the rename, so numbers compare across it).
func benchKey(i int) string {
	return fmt.Sprintf("root%d:owner:%d:view%d", i%benchRoots, i, i%4)
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
		s.DeletePrefix("root7:owner:nope:")
	}
}

func BenchmarkDeletePrefixMissNaive(b *testing.B) {
	s := benchStore()
	for b.Loop() {
		naiveDeletePrefix(s, "root7:owner:nope:")
	}
}

// expiringStore is 100,000 keys: the first `due` expire at at, the rest a day of seconds later.
func expiringStore(at time.Time, due int) *Store {
	s := New()
	for i := range benchKeys {
		exp := at
		if i >= due {
			exp = at.Add(time.Hour + time.Duration(i%86_400)*time.Second)
		}
		s.Set(names[i], Entry{Value: val("v"), ExpiresAt: exp})
	}
	return s
}

// BenchmarkStoreExpireDue: one Expire pass removing 10,000 due keys of 100,000 (D-045 target).
func BenchmarkStoreExpireDue(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		s := expiringStore(t0, 10_000)
		b.StartTimer()
		if n := len(s.Expire(t0.Add(time.Millisecond))); n != 10_000 {
			b.Fatalf("expired %d", n)
		}
	}
}

// BenchmarkStoreExpireIdle: an Expire pass with nothing due, one janitor interval (60 s) after the
// last, over 100,000 keys with expiries: the time the sheet mutex is held on a quiet sheet.
func BenchmarkStoreExpireIdle(b *testing.B) {
	s := expiringStore(t0.AddDate(100, 0, 0), 0) // far ahead: b.N minutes never reach them
	now := t0
	s.Expire(now)
	for b.Loop() {
		now = now.Add(time.Minute)
		if n := len(s.Expire(now)); n != 0 {
			b.Fatalf("expired %d", n)
		}
	}
}
