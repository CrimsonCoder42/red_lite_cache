// Covers: backendAdvo cache layer
package conformance

// B1 (D-037): the profile sweep DeletePrefix("<root>:patient:<uuid>") over corpus-shaped keys,
// first/last-segment index alone vs that index plus the patient-head index. It imports
// internal/cache directly: internal/ is importable from anywhere inside this module and
// testing/conformance is in the module; only this benchmark does it (every C test uses the public
// reader). The keys are keycorpus.go's real shapes (b1Keys below).

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
)

const (
	b1Profiles = 50
	b1Total    = 10_000
)

// b1Profile is the i-th profile uuid, deterministic so every run sweeps the same keys.
func b1Profile(i int) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "b1-profile-%d", i))
}

// b1PerHead is how many corpus keys each (profile, root) head holds: 10 000 / (50 × 8).
const b1PerHead = b1Total / (b1Profiles * 8)

// b1Keys is ≈10 000 REAL key shapes (keycorpus.go, D-033): the 8 profile roots with the most
// corpus shapes × 50 profiles, up to b1PerHead keys per head drawn from KeysFor (every shape of
// the root, then more seeds of each), plus every profile-independent record key once. The
// returned root is the one the sweep benchmark targets (the largest).
func b1Keys() (keys []string, sweepRoot string) {
	tenant := b1Profile(-1)
	byRoot := map[string][]string{}
	for _, ck := range KeysFor(b1Profile(0), tenant, b1PerHead) {
		if ck.Shape.ProfileScoped {
			byRoot[ck.Shape.Root] = append(byRoot[ck.Shape.Root], ck.Key)
		}
	}
	roots := Roots()
	slices.SortStableFunc(roots, func(a, b string) int { return len(byRoot[b]) - len(byRoot[a]) })
	roots = roots[:min(8, len(roots))]
	chosen := map[string]bool{}
	for _, r := range roots {
		chosen[r] = true
	}
	for p := range b1Profiles {
		perHead := map[string]int{}
		for _, ck := range KeysFor(b1Profile(p), tenant, b1PerHead) {
			s := ck.Shape
			switch {
			case !s.ProfileScoped:
				if p == 0 {
					keys = append(keys, ck.Key) // records: profile-independent, present once
				}
			case chosen[s.Root] && perHead[s.Root] < b1PerHead:
				perHead[s.Root]++
				keys = append(keys, ck.Key)
			}
		}
	}
	return keys, roots[0]
}

var b1Variants = []struct {
	name string
	new  func() *cache.Store
}{
	{"index=segment", cache.NewWithoutHeadIndex},
	{"index=head", cache.New},
}

func b1Store(newStore func() *cache.Store, keys []string) *cache.Store {
	s := newStore()
	e := cache.Entry{Value: []byte(`"v"`)}
	for _, k := range keys {
		s.Set(k, e)
	}
	return s
}

// BenchmarkB1 sweeps one profile's head in one root (25 of 10 000 keys), restored outside the timer.
func BenchmarkB1(b *testing.B) {
	keys, root := b1Keys()
	b.Logf("B1 corpus: %d keys, sweep root %q", len(keys), root)
	prefix := DomainPatientPrefix(root, b1Profile(7))
	var swept []string
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			swept = append(swept, k)
		}
	}
	e := cache.Entry{Value: []byte(`"v"`)}
	for _, v := range b1Variants {
		b.Run("sweep/"+v.name, func(b *testing.B) {
			s := b1Store(v.new, keys)
			for b.Loop() {
				if n := s.DeletePrefix(prefix); n != len(swept) {
					b.Fatalf("deleted %d, want %d", n, len(swept))
				}
				b.StopTimer()
				for _, k := range swept {
					s.Set(k, e)
				}
				b.StartTimer()
			}
		})
		// invalidateDomainBoth's requester sweep: a profile with no keys in a populated sheet.
		b.Run("miss/"+v.name, func(b *testing.B) {
			s := b1Store(v.new, keys)
			miss := DomainPatientPrefix(root, b1Profile(b1Profiles+1))
			for b.Loop() {
				if n := s.DeletePrefix(miss); n != 0 {
					b.Fatalf("deleted %d, want 0", n)
				}
			}
		})
		// The index's write cost: a new key set then deleted (both maintain every index).
		b.Run("churn/"+v.name, func(b *testing.B) {
			s := b1Store(v.new, keys)
			k := DomainPatientPrefix(root, b1Profile(3)) + ":churn"
			for b.Loop() {
				s.Set(k, e)
				s.Delete(k)
			}
		})
	}
}
