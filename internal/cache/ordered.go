package cache

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ordered is a B+tree set of distinct strings in byte order (S9 §2): internal nodes hold
// separators, leaves hold the keys and are chained left to right, so a prefix group or a range is
// one descent plus a walk along the chain. Removal rebalances (borrow from a sibling, else merge),
// so no node but the root holds fewer than minKeys keys and an emptied set holds no node at all.
// Not safe for concurrent use: the owner's lock guards it (the store's mu, the reader's local.mu).
//
// Pattern A of the S9 request (first and last position by binary search) is the descent; pattern
// B (cut the range) is the walk; pattern C (build from sorted input) is build.
type ordered struct {
	root    *bnode
	n       int // keys
	nodes   int // allocated nodes (leaves and internal)
	maxKeys int // a node splits above this
	minKeys int // a non-root node below this borrows or merges
}

type bnode struct {
	// Leaf: the keys, ascending. Internal: separators; kids[i] holds keys < keys[i] <= kids[i+1]'s.
	keys []string
	kids []*bnode // nil for a leaf
	next *bnode   // leaf chain
}

func (n *bnode) leaf() bool { return n.kids == nil }

func newOrdered(maxKeys, minKeys int) *ordered {
	return &ordered{maxKeys: maxKeys, minKeys: minKeys}
}

func (t *ordered) len() int { return t.n }

func (t *ordered) reset() { t.root, t.n, t.nodes = nil, 0, 0 }

// childIdx is the kid of internal node n that holds k: the number of separators <= k.
func childIdx(n *bnode, k string) int {
	return sort.Search(len(n.keys), func(i int) bool { return n.keys[i] > k })
}

// insert adds k, which must not be present.
func (t *ordered) insert(k string) {
	if t.root == nil {
		t.root = &bnode{keys: make([]string, 0, 8)}
		t.nodes = 1
	}
	if sep, right := t.ins(t.root, k); right != nil {
		t.root = &bnode{keys: []string{sep}, kids: []*bnode{t.root, right}}
		t.nodes++
	}
	t.n++
}

// ins inserts k under n and returns the separator and new right sibling when n split.
func (t *ordered) ins(n *bnode, k string) (string, *bnode) {
	if n.leaf() {
		i := sort.SearchStrings(n.keys, k)
		n.keys = slices.Insert(n.keys, i, k)
		if len(n.keys) <= t.maxKeys {
			return "", nil
		}
		m := len(n.keys) / 2
		r := &bnode{keys: append(make([]string, 0, t.maxKeys+1), n.keys[m:]...), next: n.next}
		clear(n.keys[m:]) // the moved strings must not stay reachable from the old backing array
		n.keys = n.keys[:m]
		n.next = r
		t.nodes++
		return r.keys[0], r
	}
	i := childIdx(n, k)
	sep, right := t.ins(n.kids[i], k)
	if right == nil {
		return "", nil
	}
	n.keys = slices.Insert(n.keys, i, sep)
	n.kids = slices.Insert(n.kids, i+1, right)
	if len(n.keys) <= t.maxKeys {
		return "", nil
	}
	m := len(n.keys) / 2
	up := n.keys[m]
	r := &bnode{
		keys: append(make([]string, 0, t.maxKeys+1), n.keys[m+1:]...),
		kids: append(make([]*bnode, 0, t.maxKeys+2), n.kids[m+1:]...),
	}
	clear(n.keys[m:])
	clear(n.kids[m+1:])
	n.keys, n.kids = n.keys[:m], n.kids[:m+1]
	t.nodes++
	return up, r
}

// remove deletes k and reports whether it was present.
func (t *ordered) remove(k string) bool {
	if t.root == nil || !t.del(t.root, k) {
		return false
	}
	t.n--
	switch {
	case t.n == 0:
		t.reset()
	case !t.root.leaf() && len(t.root.kids) == 1:
		t.root = t.root.kids[0]
		t.nodes--
	}
	return true
}

func (t *ordered) del(n *bnode, k string) bool {
	if n.leaf() {
		i := sort.SearchStrings(n.keys, k)
		if i == len(n.keys) || n.keys[i] != k {
			return false
		}
		n.keys = slices.Delete(n.keys, i, i+1)
		return true
	}
	i := childIdx(n, k)
	if !t.del(n.kids[i], k) {
		return false
	}
	if len(n.kids[i].keys) < t.minKeys {
		t.rebalance(n, i)
	}
	return true
}

// rebalance fixes parent's underfull kid i: borrow one key from a sibling that can spare it, else
// merge with a sibling (the parent loses a separator and may underflow in turn, which its own
// parent fixes on the way up).
func (t *ordered) rebalance(p *bnode, i int) {
	c := p.kids[i]
	if i > 0 && len(p.kids[i-1].keys) > t.minKeys {
		l := p.kids[i-1]
		if c.leaf() {
			last := l.keys[len(l.keys)-1]
			l.keys = slices.Delete(l.keys, len(l.keys)-1, len(l.keys))
			c.keys = slices.Insert(c.keys, 0, last)
			p.keys[i-1] = c.keys[0]
		} else {
			c.keys = slices.Insert(c.keys, 0, p.keys[i-1])
			c.kids = slices.Insert(c.kids, 0, l.kids[len(l.kids)-1])
			p.keys[i-1] = l.keys[len(l.keys)-1]
			l.keys = slices.Delete(l.keys, len(l.keys)-1, len(l.keys))
			l.kids = slices.Delete(l.kids, len(l.kids)-1, len(l.kids))
		}
		return
	}
	if i+1 < len(p.kids) && len(p.kids[i+1].keys) > t.minKeys {
		r := p.kids[i+1]
		if c.leaf() {
			c.keys = append(c.keys, r.keys[0])
			r.keys = slices.Delete(r.keys, 0, 1)
			p.keys[i] = r.keys[0]
		} else {
			c.keys = append(c.keys, p.keys[i])
			c.kids = append(c.kids, r.kids[0])
			p.keys[i] = r.keys[0]
			r.keys = slices.Delete(r.keys, 0, 1)
			r.kids = slices.Delete(r.kids, 0, 1)
		}
		return
	}
	// Merge: fold the right one of the pair (c and a sibling) into the left one.
	l, r, sep := i-1, i, i-1
	if i == 0 {
		l, r, sep = 0, 1, 0
	}
	if r >= len(p.kids) { // only child: nothing to merge with (root fixes itself)
		return
	}
	left, right := p.kids[l], p.kids[r]
	if left.leaf() {
		left.keys = append(left.keys, right.keys...)
		left.next = right.next
	} else {
		left.keys = append(append(left.keys, p.keys[sep]), right.keys...)
		left.kids = append(left.kids, right.kids...)
	}
	p.keys = slices.Delete(p.keys, sep, sep+1)
	p.kids = slices.Delete(p.kids, r, r+1)
	t.nodes--
}

// lowerBound returns the leaf and position of the first key >= k (leaf nil when none).
func (t *ordered) lowerBound(k string) (*bnode, int) {
	n := t.root
	if n == nil {
		return nil, 0
	}
	for !n.leaf() {
		n = n.kids[childIdx(n, k)]
	}
	i := sort.SearchStrings(n.keys, k)
	for n != nil && i == len(n.keys) {
		n, i = n.next, 0
	}
	return n, i
}

// appendPrefix appends every key starting with p (p != "") in order.
func (t *ordered) appendPrefix(dst []string, p string) []string {
	for n, i := t.lowerBound(p); n != nil; n, i = n.next, 0 {
		for ; i < len(n.keys); i++ {
			if !strings.HasPrefix(n.keys[i], p) {
				return dst
			}
			dst = append(dst, n.keys[i])
		}
	}
	return dst
}

// cutPrefix removes every key starting with p (p != "") and appends them, in order, to dst.
func (t *ordered) cutPrefix(dst []string, p string) []string {
	// Fast path, the common case of a small group: the whole group lies in one leaf and the leaf
	// keeps at least minKeys, so the run is cut with one copy and no rebalance (pattern B) — one
	// descent instead of one per key. Separators above stay valid bounds.
	if n, i := t.lowerBound(p); n != nil {
		j := i
		for j < len(n.keys) && strings.HasPrefix(n.keys[j], p) {
			j++
		}
		ends := j < len(n.keys) || n.next == nil || !strings.HasPrefix(n.next.keys[0], p)
		if ends && (n == t.root || len(n.keys)-(j-i) >= t.minKeys) {
			dst = append(dst, n.keys[i:j]...)
			n.keys = slices.Delete(n.keys, i, j)
			t.n -= j - i
			if t.n == 0 {
				t.reset()
			}
			return dst
		}
	}
	start := len(dst)
	dst = t.appendPrefix(dst, p)
	if len(dst)-start == t.n { // the whole set: drop it in one step
		t.reset()
		return dst
	}
	for _, k := range dst[start:] {
		t.remove(k)
	}
	return dst
}

// scan appends up to count keys >= from, in order.
func (t *ordered) scan(dst []string, from string, count int) []string {
	for n, i := t.lowerBound(from); n != nil && count > 0; n, i = n.next, 0 {
		for ; i < len(n.keys) && count > 0; i, count = i+1, count-1 {
			dst = append(dst, n.keys[i])
		}
	}
	return dst
}

// build replaces the content with sorted (distinct, ascending) in one pass, bottom up (pattern
// C): leaves filled to fill keys, parents built from the children's first keys, with the last
// group of each level evened out with its neighbour so no node is left under minKeys.
func (t *ordered) build(sorted []string) {
	t.reset()
	if len(sorted) == 0 {
		return
	}
	fill := max(t.maxKeys*3/4, t.minKeys, 1)
	var level []*bnode
	var prev *bnode
	for _, g := range groupsOf(len(sorted), fill, t.minKeys, t.maxKeys) {
		n := &bnode{keys: append(make([]string, 0, t.maxKeys+1), sorted[g[0]:g[1]]...)}
		if prev != nil {
			prev.next = n
		}
		prev = n
		level = append(level, n)
	}
	t.nodes = len(level)
	firstKey := func(n *bnode) string {
		for !n.leaf() {
			n = n.kids[0]
		}
		return n.keys[0]
	}
	for len(level) > 1 {
		var up []*bnode
		for _, g := range groupsOf(len(level), fill+1, t.minKeys+1, t.maxKeys+1) {
			kids := level[g[0]:g[1]]
			n := &bnode{kids: append(make([]*bnode, 0, t.maxKeys+2), kids...), keys: make([]string, 0, t.maxKeys+1)}
			for _, c := range kids[1:] {
				n.keys = append(n.keys, firstKey(c))
			}
			up = append(up, n)
		}
		t.nodes += len(up)
		level = up
	}
	t.root, t.n = level[0], len(sorted)
}

// groupsOf splits n items into [lo, hi) runs of size fill. When the last run would hold fewer than
// minSize, the last two runs become one (if they fit in capacity) or are evened out.
func groupsOf(n, fill, minSize, capacity int) [][2]int {
	var out [][2]int
	for lo := 0; lo < n; lo += fill {
		out = append(out, [2]int{lo, min(lo+fill, n)})
	}
	if k := len(out); k >= 2 && out[k-1][1]-out[k-1][0] < minSize {
		lo, hi := out[k-2][0], out[k-1][1]
		if hi-lo <= capacity {
			return append(out[:k-2], [2]int{lo, hi})
		}
		mid := lo + (hi-lo)/2
		out[k-2], out[k-1] = [2]int{lo, mid}, [2]int{mid, hi}
	}
	return out
}

// check verifies every invariant: order across leaves, separators bound their subtrees, no
// non-root node under minKeys or over maxKeys, all leaves at one depth, the chain visits every
// leaf, and the counters. For tests.
func (t *ordered) check() error {
	if t.root == nil {
		if t.n != 0 || t.nodes != 0 {
			return fmt.Errorf("empty tree with n=%d nodes=%d", t.n, t.nodes)
		}
		return nil
	}
	var leaves []*bnode
	nodes, keys, depth := 0, 0, -1
	var walk func(n *bnode, lo, hi *string, d int) error
	walk = func(n *bnode, lo, hi *string, d int) error {
		nodes++
		if n != t.root && (len(n.keys) < t.minKeys || len(n.keys) > t.maxKeys) {
			return fmt.Errorf("node with %d keys (min %d max %d)", len(n.keys), t.minKeys, t.maxKeys)
		}
		for i, k := range n.keys {
			if (lo != nil && k < *lo) || (hi != nil && k >= *hi) || (i > 0 && n.keys[i-1] >= k) {
				return fmt.Errorf("key %q out of order or bounds", k)
			}
		}
		if n.leaf() {
			if depth == -1 {
				depth = d
			} else if depth != d {
				return fmt.Errorf("leaves at depths %d and %d", depth, d)
			}
			leaves = append(leaves, n)
			keys += len(n.keys)
			return nil
		}
		if len(n.kids) != len(n.keys)+1 {
			return fmt.Errorf("internal node with %d keys and %d kids", len(n.keys), len(n.kids))
		}
		for i, c := range n.kids {
			l, h := lo, hi
			if i > 0 {
				l = &n.keys[i-1]
			}
			if i < len(n.keys) {
				h = &n.keys[i]
			}
			if err := walk(c, l, h, d+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(t.root, nil, nil, 0); err != nil {
		return err
	}
	for i, l := range leaves {
		var want *bnode
		if i+1 < len(leaves) {
			want = leaves[i+1]
		}
		if l.next != want {
			return fmt.Errorf("leaf chain broken at leaf %d", i)
		}
	}
	if keys != t.n || nodes != t.nodes {
		return fmt.Errorf("counted %d keys %d nodes, tree says %d / %d", keys, nodes, t.n, t.nodes)
	}
	return nil
}
