// Covers: internal/cache
package cache

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func val(s string) json.RawMessage { return json.RawMessage(`"` + s + `"`) }

func TestLazyExpiry(t *testing.T) {
	s := New()
	s.Set("a", Entry{Value: val("1"), ExpiresAt: t0.Add(time.Minute)})
	if _, ok := s.Get("a", t0.Add(59*time.Second)); !ok {
		t.Fatal("live entry missed")
	}
	if _, ok := s.Get("a", t0.Add(time.Minute)); ok {
		t.Fatal("expired entry served by Get")
	}
	if s.Has("a", t0.Add(2*time.Minute)) {
		t.Fatal("expired entry served by Has")
	}
	if got := s.All(t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("All returned expired entries: %v", got)
	}
	if keys := s.Expire(t0.Add(time.Minute)); !slices.Equal(keys, []string{"a"}) {
		t.Fatalf("Expire = %v, want [a]", keys)
	}
	if s.Len() != 0 {
		t.Fatalf("Len after Expire = %d", s.Len())
	}
}

// K5: in advo_cache an old Set's sleeping goroutine deleted a refreshed value. Here a re-set
// replaces the expiry, and no earlier deadline survives.
func TestResetExtendsLife(t *testing.T) {
	s := New()
	s.Set("a", Entry{Value: val("old"), ExpiresAt: t0.Add(time.Minute)})
	if isNew := s.Set("a", Entry{Value: val("new"), ExpiresAt: t0.Add(10 * time.Minute)}); isNew {
		t.Fatal("re-set reported new")
	}
	if keys := s.Expire(t0.Add(2 * time.Minute)); len(keys) != 0 {
		t.Fatalf("refreshed key expired by the old deadline: %v", keys)
	}
	e, ok := s.Get("a", t0.Add(2*time.Minute))
	if !ok || string(e.Value) != `"new"` {
		t.Fatalf("Get = %s %v, want new", e.Value, ok)
	}
}

func TestNoExpiry(t *testing.T) {
	s := New()
	s.Set("a", Entry{Value: val("1")})
	far := t0.Add(1000 * time.Hour)
	if !s.Has("a", far) || len(s.Expire(far)) != 0 {
		t.Fatal("zero ExpiresAt expired")
	}
	if (Policy{}).ExpiresAt(t0, 0) != (time.Time{}) {
		t.Fatal("DefaultTTL 0 and ttl 0 must be no expiry")
	}
}

func TestPolicy(t *testing.T) {
	p := Policy{DefaultTTL: 15 * time.Minute}
	if got := p.ExpiresAt(t0, 0); !got.Equal(t0.Add(15 * time.Minute)) {
		t.Fatalf("default ttl: %v", got)
	}
	if got := p.ExpiresAt(t0, 2); !got.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("ttl minutes: %v", got)
	}
	if got := (Policy{}).ExpiresAt(t0, 3); !got.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("explicit ttl with no default: %v", got)
	}
	j := Policy{DefaultTTL: 10 * time.Minute, JitterPercent: 20}
	lo, hi := t0.Add(10*time.Minute), t0.Add(12*time.Minute)
	sawJitter := false
	for range 2000 {
		got := j.ExpiresAt(t0, 0)
		if got.Before(lo) || got.After(hi) {
			t.Fatalf("jitter out of bounds: %v not in [%v,%v]", got, lo, hi)
		}
		if got.After(lo) {
			sawJitter = true
		}
	}
	if !sawJitter {
		t.Fatal("jitter never added")
	}
}

// naive is the reference model: a plain map the index must agree with.
type naive map[string]bool

func (n naive) delete(match func(string) bool) int {
	c := 0
	for k := range n {
		if match(k) {
			delete(n, k)
			c++
		}
	}
	return c
}

func keysOf(s *Store) []string {
	var out []string
	for k := range s.All(t0) {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func modelKeys(n naive) []string {
	var out []string
	for k := range n {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// checkIndex asserts the index holds exactly the map's keys, in order, with every tree invariant,
// and no node at all once the store is empty.
func checkIndex(t *testing.T, s *Store) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := make([]string, 0, len(s.m))
	for k := range s.m {
		want = append(want, k)
	}
	slices.Sort(want)
	checkGroups(t, s.groups, want)
}

func TestPrefixSuffixCases(t *testing.T) {
	keys := []string{"user:1", "user:2:permissions", "users:9", "use", "u", "plain", "a:owner:x:permissions",
		"a:owner:x:view", "a:owner:y:view", "b:owner:x:view", "permissions", "x:", ":lead", ""}
	cases := []struct {
		name   string
		prefix bool
		arg    string
	}{
		{"prefix empty", true, ""},
		{"prefix mid first segment", true, "use"},
		{"prefix whole root", true, "user"},
		{"prefix root colon", true, "user:"},
		{"prefix deep", true, "a:owner:x:"},
		{"prefix no colon key", true, "pla"},
		{"prefix leading colon", true, ":"},
		{"prefix missing", true, "zzz:"},
		{"suffix empty", false, ""},
		{"suffix colon segment", false, ":permissions"},
		{"suffix mid last segment", false, "ions"},
		{"suffix whole key", false, "permissions"},
		{"suffix deep", false, ":x:view"},
		{"suffix trailing colon", false, ":"},
		{"suffix missing", false, ":nothing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, m := New(), naive{}
			for _, k := range keys {
				s.Set(k, Entry{Value: val(k)})
				m[k] = true
			}
			var got, want int
			if c.prefix {
				got = s.DeletePrefix(c.arg)
				want = m.delete(func(k string) bool { return strings.HasPrefix(k, c.arg) })
			} else {
				got = s.DeleteSuffix(c.arg)
				want = m.delete(func(k string) bool { return strings.HasSuffix(k, c.arg) })
			}
			if got != want {
				t.Fatalf("deleted %d, want %d", got, want)
			}
			if !slices.Equal(keysOf(s), modelKeys(m)) {
				t.Fatalf("left %v, want %v", keysOf(s), modelKeys(m))
			}
			checkIndex(t, s)
		})
	}
}

// TestIndexRandomOps drives Set/Delete/DeletePrefix/DeleteSuffix/Expire/Replace at random against
// the naive model and checks the indexes after every op.
func TestIndexRandomOps(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	segs := []string{"", "a", "ab", "user", "users", "owner", "permissions", "view", "x"}
	randKey := func() string {
		n := r.IntN(4) + 1
		parts := make([]string, n)
		for i := range parts {
			parts[i] = segs[r.IntN(len(segs))]
		}
		return strings.Join(parts, ":")
	}
	randPattern := func() string {
		k := randKey()
		return k[:r.IntN(len(k)+1)]
	}
	s := New()
	m := map[string]Entry{}
	now := t0
	for i := range 5000 {
		switch op := r.IntN(10); {
		case op < 5:
			k := randKey()
			e := Entry{Value: val(k)}
			if r.IntN(3) == 0 {
				e.ExpiresAt = now.Add(time.Duration(r.IntN(5)) * time.Second)
			}
			s.Set(k, e)
			m[k] = e
		case op == 5:
			k := randKey()
			_, had := m[k]
			if s.Delete(k) != had {
				t.Fatalf("op %d: Delete(%q) disagreed", i, k)
			}
			delete(m, k)
		case op == 6:
			p := randPattern()
			want := 0
			for k := range m {
				if strings.HasPrefix(k, p) {
					delete(m, k)
					want++
				}
			}
			if got := s.DeletePrefix(p); got != want {
				t.Fatalf("op %d: DeletePrefix(%q) = %d, want %d", i, p, got, want)
			}
		case op == 7:
			k := randKey()
			p := k[r.IntN(len(k)+1):]
			want := 0
			for k := range m {
				if strings.HasSuffix(k, p) {
					delete(m, k)
					want++
				}
			}
			if got := s.DeleteSuffix(p); got != want {
				t.Fatalf("op %d: DeleteSuffix(%q) = %d, want %d", i, p, got, want)
			}
		case op == 8:
			now = now.Add(time.Second)
			got := s.Expire(now)
			var want []string
			for k, e := range m {
				if e.Expired(now) {
					want = append(want, k)
					delete(m, k)
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("op %d: Expire = %v, want %v", i, got, want)
			}
		default:
			if r.IntN(20) == 0 {
				snap := map[string]Entry{}
				for range r.IntN(20) {
					k := randKey()
					snap[k] = Entry{Value: val(k)}
				}
				s.Replace(snap)
				m = snap
			}
		}
		if s.Len() != len(m) {
			t.Fatalf("op %d: Len %d, model %d", i, s.Len(), len(m))
		}
		checkIndex(t, s)
	}
}

// TestPrefixShapes: prefixes ending at, inside and past every segment of a scoped key set agree
// with the naive model and leave the index consistent. Before D-044 these shapes chose between the
// segment and head indexes (D-037); the ordered index treats them all alike.
func TestPrefixShapes(t *testing.T) {
	keys := []string{
		"vit:owner:u1", "vit:owner:u1:bp", "vit:owner:u1:bp:feed:1:2", "vit:owner:u10:bp",
		"vit:owner:u2:bp", "vit:owner:", "vit:owner::x", "vit:owners:u1:bp", "vit:user:u1:record",
		"vit", "inf:owner:u1:diaper", "inf:owner:u1", "user:t1:permissions", "owner:u1:x",
	}
	prefixes := []string{
		"vit:owner:u1",         // three whole segments, no trailing ':': also u10
		"vit:owner:u1:",        // three segments + ':'
		"vit:owner:u1:bp",      // longer
		"vit:owner:u1:bp:feed", // longer still
		"vit:owner:u",          // mid third segment
		"vit:owner:",           // empty third segment
		"vit:owner",            // mid second segment
		"vit:owner::",          // empty third segment, closed
		"vit:user:u1",          // another second segment
		"vit:owner:nope",       // absent
		"inf:owner:u1",
		"owner:u1",
		"vi",
	}
	{
		for _, p := range prefixes {
			t.Run(p, func(t *testing.T) {
				s, m := New(), naive{}
				for _, k := range keys {
					s.Set(k, Entry{Value: val(k)})
					m[k] = true
				}
				checkIndex(t, s)
				got := s.DeletePrefix(p)
				want := m.delete(func(k string) bool { return strings.HasPrefix(k, p) })
				if got != want {
					t.Fatalf("DeletePrefix(%q) = %d, want %d", p, got, want)
				}
				if !slices.Equal(keysOf(s), modelKeys(m)) {
					t.Fatalf("left %v, want %v", keysOf(s), modelKeys(m))
				}
				checkIndex(t, s)
			})
		}
	}
}

// TestIndexLifecycle: the index follows delete, expire, re-set, Replace and clear, and holds no
// node once the store is empty.
func TestIndexLifecycle(t *testing.T) {
	s := New()
	s.Set("r:owner:a:v1", Entry{Value: val("1"), ExpiresAt: t0.Add(time.Minute)})
	s.Set("r:owner:a:v2", Entry{Value: val("2")})
	s.Set("r:owner:b:v1", Entry{Value: val("3")})
	checkIndex(t, s)
	if s.Set("r:owner:a:v2", Entry{Value: val("2b")}) {
		t.Fatal("re-set reported new")
	}
	checkIndex(t, s)
	if keys := s.Expire(t0.Add(time.Minute)); !slices.Equal(keys, []string{"r:owner:a:v1"}) {
		t.Fatalf("Expire = %v", keys)
	}
	checkIndex(t, s)
	if !s.Delete("r:owner:a:v2") {
		t.Fatal("Delete missed")
	}
	checkIndex(t, s)
	if got := s.DeletePrefix("r:owner:a"); got != 0 {
		t.Fatalf("DeletePrefix after the group emptied = %d", got)
	}
	s.Replace(map[string]Entry{"q:owner:z:v": {Value: val("z")}})
	checkIndex(t, s)
	if got := s.DeletePrefix("r:"); got != 0 {
		t.Fatalf("Replace kept the old index: deleted %d", got)
	}
	if got := s.DeletePrefix("q:owner:z"); got != 1 {
		t.Fatalf("index not rebuilt by Replace: deleted %d", got)
	}
	s.Set("q:owner:z:v", Entry{Value: val("z")})
	if got := s.DeletePrefix(""); got != 1 {
		t.Fatalf("clear: deleted %d", got)
	}
	checkIndex(t, s)
	if st := s.Stats(); st != (Stats{}) {
		t.Fatalf("emptied store keeps %+v", st)
	}
}

func TestReplaceCopies(t *testing.T) {
	s := New()
	s.Set("old:1", Entry{Value: val("x")})
	in := map[string]Entry{"n:1": {Value: val("1")}, "n:2": {Value: val("2")}}
	s.Replace(in)
	delete(in, "n:1")
	if s.Has("old:1", t0) || !s.Has("n:1", t0) || s.Len() != 2 {
		t.Fatalf("Replace: keys %v", keysOf(s))
	}
	if got := s.DeletePrefix("n:"); got != 2 {
		t.Fatalf("index not rebuilt by Replace: deleted %d", got)
	}
}

func ExampleStore_DeleteSuffix() {
	s := New()
	s.Set("root:owner:1:permissions", Entry{Value: val("p")})
	s.Set("root:owner:1:view", Entry{Value: val("v")})
	fmt.Println(s.DeleteSuffix(":permissions"), s.Len())
	// Output: 1 1
}
