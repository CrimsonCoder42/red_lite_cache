package cache

import (
	"fmt"
	"slices"
)

// Groups is the index behind prefix and suffix deletes (R7), shared by the server's Store and the
// reader's local map (reader/local.go) so both sides decide a group the same way (S9 §2, D-044).
// It holds keys only; the caller owns the values and the lock.
//
// Two B+trees: fwd holds the keys, rev holds every key byte-reversed.
//   - The prefix group of p (p != "") is every key k with p <= k < prefixEnd(p): exactly the keys
//     starting with p, found by one descent in fwd and a walk along its leaves.
//   - The suffix group of s (s != "") is every key whose reversal is in the prefix group of
//     reverse(s) in rev: exactly the keys ending with s, at any boundary.
//
// Order is by bytes (Go string comparison); nothing about separators, segment names or key shapes
// is assumed. Not safe for concurrent use.
type Groups struct {
	fwd, rev *ordered
	buf      []string // reused by CutSuffix for the reversed matches
}

// Node size, chosen by the step-1 sweep (doc/ordered_index_build_2026-11-11/REPORT.md): a node
// splits above DefaultNodeKeys keys, and a non-root node below DefaultMinKeys borrows or merges.
const (
	DefaultNodeKeys = 128
	DefaultMinKeys  = 32
)

// NewGroups returns an empty index with the default node size.
func NewGroups() *Groups { return newGroupsSized(DefaultNodeKeys, DefaultMinKeys) }

func newGroupsSized(maxKeys, minKeys int) *Groups {
	return &Groups{fwd: newOrdered(maxKeys, minKeys), rev: newOrdered(maxKeys, minKeys)}
}

// reverseKey is s with its bytes in reverse order.
func reverseKey(s string) string {
	b := make([]byte, len(s))
	for i := range len(s) {
		b[len(s)-1-i] = s[i]
	}
	return string(b)
}

// prefixEnd is the smallest string greater than every string starting with p, or "" when there is
// none (p is empty or all 0xFF bytes): trailing 0xFF bytes are dropped and the last byte is
// incremented. [p, prefixEnd(p)) is exactly the strings starting with p.
func prefixEnd(p string) string {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// Len is the number of keys.
func (g *Groups) Len() int { return g.fwd.len() }

// Add indexes key, which must not be indexed yet.
func (g *Groups) Add(key string) {
	g.fwd.insert(key)
	g.rev.insert(reverseKey(key))
}

// Remove drops key, which must be indexed.
func (g *Groups) Remove(key string) {
	g.fwd.remove(key)
	g.rev.remove(reverseKey(key))
}

// Reset empties the index and frees every node.
func (g *Groups) Reset() {
	g.fwd.reset()
	g.rev.reset()
	g.buf = nil
}

// CutPrefix removes every key starting with prefix (not "": a clear is the caller's Reset) and
// appends them to dst in byte order.
func (g *Groups) CutPrefix(dst []string, prefix string) []string {
	start := len(dst)
	dst = g.fwd.cutPrefix(dst, prefix)
	if g.fwd.len() == 0 {
		g.rev.reset()
		return dst
	}
	for _, k := range dst[start:] {
		g.rev.remove(reverseKey(k))
	}
	return dst
}

// CutSuffix removes every key ending with suffix (not "") and appends them to dst, in the order
// of their reversals.
func (g *Groups) CutSuffix(dst []string, suffix string) []string {
	g.buf = g.rev.cutPrefix(g.buf[:0], reverseKey(suffix))
	whole := g.rev.len() == 0
	for _, rk := range g.buf {
		k := reverseKey(rk)
		if !whole {
			g.fwd.remove(k)
		}
		dst = append(dst, k)
	}
	if whole {
		g.fwd.reset()
	}
	clear(g.buf) // drop the key strings the buffer still points at
	if cap(g.buf) > 1024 {
		g.buf = nil
	}
	return dst
}

// Scan appends up to n keys >= from, in byte order.
func (g *Groups) Scan(dst []string, from string, n int) []string { return g.fwd.scan(dst, from, n) }

// Load replaces the content with keys (distinct, any order): both trees are built bottom up from
// sorted copies in one pass each, instead of key-by-key inserts (replica snapshot, hydration, a
// reader's sheet_state). keys is not modified.
func (g *Groups) Load(keys []string) {
	fwd := append([]string(nil), keys...)
	slices.Sort(fwd)
	g.fwd.build(fwd)
	rev := make([]string, len(keys))
	for i, k := range keys {
		rev[i] = reverseKey(k)
	}
	slices.Sort(rev)
	g.rev.build(rev)
}

// GroupsStats is the index's size: counts only (D-040). Both are 0 when the index is empty.
type GroupsStats struct {
	ForwardNodes int // B+tree nodes of the key order
	ReverseNodes int // B+tree nodes of the reversed-key order
}

// Stats counts the nodes.
func (g *Groups) Stats() GroupsStats {
	return GroupsStats{ForwardNodes: g.fwd.nodes, ReverseNodes: g.rev.nodes}
}

// check verifies both trees and that they hold the same keys. For tests.
func (g *Groups) check() error {
	if err := g.fwd.check(); err != nil {
		return err
	}
	if err := g.rev.check(); err != nil {
		return err
	}
	if g.fwd.len() != g.rev.len() {
		return fmt.Errorf("forward holds %d keys, reverse %d", g.fwd.len(), g.rev.len())
	}
	return nil
}
