package cache

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Two differently shaped generic key sets (S9: every feature proven on two shapes; nothing
// consumer-shaped in internal/).
//
//   - colon: ':'-segmented, 8 types × owner/record shapes: `tNN:owner:<id>:<view>` and, 1 in 5,
//     `tNN:rec:<id>` with the one-match suffix `:rec:<id>`.
//   - path:  '/'-separated, no ':' at all: `org-NN/site-NNN/dev-NNNNNN/m-<metric>`, with the
//     one-match suffix `/dev-NNNNNN/m-<metric>`.
type keySetT struct {
	keys   []string
	suffix []string // per key: a suffix matching only that key, or ""
}

var (
	colonViews   = []string{"list", "count", "summary", "detail", "timeline"}
	pathMetrics  = []string{"temp", "rh", "co2", "pm25", "volt", "rssi"}
	genericSets  = map[string]func(int) keySetT{"colon": colonKeys, "path": pathKeys}
	genericNames = []string{"colon", "path"}
)

func gid(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i) }

func colonKeys(n int) keySetT {
	ks := keySetT{keys: make([]string, n), suffix: make([]string, n)}
	p := 0
	for i := range n {
		if i%5 == 4 {
			ks.keys[i] = fmt.Sprintf("t%02d:rec:%s", i%8, gid(i))
			ks.suffix[i] = ":rec:" + gid(i)
			continue
		}
		ks.keys[i] = fmt.Sprintf("t%02d:owner:%s:%s", p%8, gid(p/40), colonViews[(p/8)%5])
		p++
	}
	return ks
}

func pathKeys(n int) keySetT {
	ks := keySetT{keys: make([]string, n), suffix: make([]string, n)}
	for i := range n {
		tail := fmt.Sprintf("/dev-%06d/m-%s", i/6, pathMetrics[i%6])
		ks.keys[i] = fmt.Sprintf("org-%02d/site-%03d", i%37, (i/37)%211) + tail
		ks.suffix[i] = tail
	}
	return ks
}

// checkGroups asserts g holds exactly want (sorted) in both orders and that its counts agree.
func checkGroups(t *testing.T, g *Groups, want []string) {
	t.Helper()
	if got := g.Scan(nil, "", len(want)+1); !slices.Equal(got, want) {
		t.Fatalf("forward scan: %d keys, want %d (first got %v want %v)", len(got), len(want), first(got), first(want))
	}
	if g.Len() != len(want) {
		t.Fatalf("Len %d, want %d", g.Len(), len(want))
	}
	if err := g.check(); err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		if st := g.Stats(); st.ForwardNodes != 0 || st.ReverseNodes != 0 {
			t.Fatalf("empty groups keep nodes: %+v", st)
		}
	}
}

// cutLen is a group pattern's length for a key of n bytes: 1 time in 20 any length (groups of any
// size, whole-set clears included), otherwise within 12 bytes of the whole key (small groups), so
// the set stays large enough for every node size to split, borrow and merge.
func cutLen(rng *rand.Rand, n int) int {
	if rng.IntN(20) == 0 {
		return 1 + rng.IntN(n)
	}
	return n - rng.IntN(min(n, 12))
}

// peakDepth is the forward tree's current depth (leaves = 1).
func peakDepth(g *Groups) int {
	d := 0
	for n := g.fwd.root; n != nil; d++ {
		if n.leaf() {
			return d + 1
		}
		n = n.kids[0]
	}
	return d
}

func first(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s[0]
}

// TestGroupsMatchNaive: 200,000 random ops per key set (S9 criterion 1): insert, remove, prefix
// group of any length, suffix group of any length, scan from any point; every result compared with
// a naive model, and both trees' invariants checked as it goes. Node size is small here so splits,
// borrows and merges happen constantly.
func TestGroupsMatchNaive(t *testing.T) {
	for _, name := range genericNames {
		for _, size := range []struct{ max, min int }{{4, 2}, {DefaultNodeKeys, DefaultMinKeys}} {
			t.Run(fmt.Sprintf("%s/node=%d", name, size.max), func(t *testing.T) {
				ks := genericSets[name](3000)
				rng := rand.New(rand.NewPCG(7, uint64(size.max)))
				g := newGroupsSized(size.max, size.min)
				model := map[string]bool{}
				sorted := func() []string {
					out := make([]string, 0, len(model))
					for k := range model {
						out = append(out, k)
					}
					slices.Sort(out)
					return out
				}
				const ops = 200_000
				peak := 0
				for op := range ops {
					k := ks.keys[rng.IntN(len(ks.keys))]
					switch r := rng.IntN(100); {
					case r < 50:
						if !model[k] {
							g.Add(k)
							model[k] = true
						}
					case r < 70:
						if model[k] {
							g.Remove(k)
							delete(model, k)
						}
					case r < 82:
						p := k[:cutLen(rng, len(k))]
						got := g.CutPrefix(nil, p)
						var want []string
						for mk := range model {
							if strings.HasPrefix(mk, p) {
								want = append(want, mk)
								delete(model, mk)
							}
						}
						slices.Sort(want)
						if !slices.Equal(got, want) {
							t.Fatalf("op %d CutPrefix(%q): got %d keys, want %d", op, p, len(got), len(want))
						}
					case r < 94:
						s := k[len(k)-cutLen(rng, len(k)):]
						got := g.CutSuffix(nil, s)
						var want []string
						for mk := range model {
							if strings.HasSuffix(mk, s) {
								want = append(want, mk)
								delete(model, mk)
							}
						}
						slices.Sort(got)
						slices.Sort(want)
						if !slices.Equal(got, want) {
							t.Fatalf("op %d CutSuffix(%q): got %d keys, want %d", op, s, len(got), len(want))
						}
					default:
						from := k[:rng.IntN(len(k)+1)]
						got := g.Scan(nil, from, 50)
						all := sorted()
						lo := sort.SearchStrings(all, from)
						if want := all[lo:min(lo+50, len(all))]; !slices.Equal(got, want) {
							t.Fatalf("op %d Scan(%q): got %v.., want %v..", op, from, first(got), first(want))
						}
					}
					peak = max(peak, len(model))
					if op%5000 == 0 {
						checkGroups(t, g, sorted())
					}
				}
				checkGroups(t, g, sorted())
				t.Logf("%d ops, peak %d keys, peak depth %d", ops, peak, peakDepth(g))
				// Load from an unsorted list must equal the incremental structure.
				want := sorted()
				shuffled := append([]string(nil), want...)
				rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
				b := newGroupsSized(size.max, size.min)
				b.Load(shuffled)
				checkGroups(t, b, want)
				// Drain to empty: every node goes (leak rule: back to 0).
				for _, k := range want {
					g.Remove(k)
				}
				checkGroups(t, g, nil)
			})
		}
	}
}

// TestBulkBuildEqualsIncremental (S9 §3): Load from unsorted keys builds the same key sets, in
// both orders, as key-by-key Adds, on both generic key sets and every size from 0 to past a node.
func TestBulkBuildEqualsIncremental(t *testing.T) {
	for _, name := range genericNames {
		for _, n := range []int{0, 1, 2, DefaultMinKeys, DefaultNodeKeys, DefaultNodeKeys + 1, 5_000} {
			ks := genericSets[name](n)
			bulk, inc := NewGroups(), NewGroups()
			rev := append([]string(nil), ks.keys...)
			slices.Reverse(rev)
			bulk.Load(rev)
			for _, k := range ks.keys {
				inc.Add(k)
			}
			want := slices.Sorted(slices.Values(ks.keys))
			checkGroups(t, bulk, want)
			checkGroups(t, inc, want)
			for i, s := range ks.suffix { // the reversed order answers the same suffix groups
				if s == "" || i%97 != 0 {
					continue
				}
				a, b := bulk.CutSuffix(nil, s), inc.CutSuffix(nil, s)
				if !slices.Equal(a, b) || len(a) != 1 {
					t.Fatalf("%s n=%d: CutSuffix(%q) bulk %v incremental %v", name, n, s, a, b)
				}
			}
		}
	}
}

// TestGroupsEdgeCases: boundaries a random walk rarely hits.
func TestGroupsEdgeCases(t *testing.T) {
	g := NewGroups()
	for _, k := range []string{"a", "a\xff", "a\xff\xff", "ab", "b", "\xff", "\xff\xff", "x:y", "y:x"} {
		g.Add(k)
	}
	if got := g.CutPrefix(nil, "a\xff"); !slices.Equal(got, []string{"a\xff", "a\xff\xff"}) {
		t.Fatalf("prefix ending in 0xff: %q", got)
	}
	if got := g.CutPrefix(nil, "\xff"); !slices.Equal(got, []string{"\xff", "\xff\xff"}) {
		t.Fatalf("all-0xff prefix (no upper bound): %q", got)
	}
	if got := g.CutSuffix(nil, "x"); !slices.Equal(got, []string{"y:x"}) {
		t.Fatalf("suffix not at a separator: %q", got)
	}
	if got := g.CutPrefix(nil, "zz"); len(got) != 0 {
		t.Fatalf("absent prefix: %q", got)
	}
	if got := g.Scan(nil, "", 10); !slices.Equal(got, []string{"a", "ab", "b", "x:y"}) {
		t.Fatalf("left: %q", got)
	}
}

func TestPrefixEnd(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a", "b"}, {"ab", "ac"}, {"a\xff", "b"}, {"\xff", ""}, {"\xff\xff", ""}, {"a:\xff\xff", "a;"},
	} {
		if got := prefixEnd(c.in); got != c.want {
			t.Errorf("prefixEnd(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
