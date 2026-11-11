# LRU cache with a linked list ("Build Your Own Cache") — distilled and checked

_Raw source: `examples/build_your_own_cache.md` (the file holds the same transcript twice). Distilled
2026-11-10. **Starting idea, not the answer** — every verdict below was checked against Go 1.27 docs
and against measurements in this repo. Cite as `doc/reference/lru_cache.md § <n>`._

**Why it matters here.** `doc/audit_2026-10-29.md` §4.3 found the reader's eviction had no policy
("first expired in 64, else arbitrary": it could evict the hottest key). This is the source that
builds the textbook fix — a true LRU on a doubly linked list plus a map — and section 6 explains why
`reader/local.go` took the *sampled* LRU instead (D-043).

## 1. Source

| | |
|---|---|
| Title | Build Your Own Cache (YouTube, Go, "intermediate level") |
| Author | Not named in the transcript; module dictated as `github.com/akil/cache-project` |
| Language / module | Go, one `main` package, no dependencies |
| Built on video | A fixed-size (5) LRU of strings: a doubly linked list with sentinel head and tail, and a `map[string]*Node` for O(1) lookup. One operation, `Check(word)`, both reads and inserts: a word already present is moved to the head, a new word is added at the head, and the tail is dropped when the length passes 5. `Display` prints the list. |
| Explicitly out of scope (his words, cleaned up) | "We won't be building the advanced operations ... the algorithms required in a cache; we'll be using just data structures." No TTL, no values separate from keys, no concurrency, no tests. |
| Framing | A teaching video on linked lists; the cache is the vehicle. Read the code as a data-structure exercise. |

---

## 2. Architecture as built

- **"True LRU", three rules (his):** an item that is used again is removed and re-added at the head;
  the order of items is kept; additions happen at the head and evictions at the tail.
- **Queue:** `Head` and `Tail` are empty sentinel nodes; `Head.Right` is the most recent item,
  `Tail.Left` the least recent. An empty queue is `Head.Right == Tail` and `Tail.Left == Head`.
- **Hash:** `map[string]*Node` from the word to its node, so "is it there, and where" is one lookup.
- **Check(word):** found → unlink the node and re-add it at the head; not found → new node at the
  head; either way the map points at it. Add evicts `Tail.Left` once `Length > SIZE`.

```
   Hash: map[string]*Node ──────────────┐
                                        ▼
   Head ⇄ [dog] ⇄ [tree] ⇄ [tomato] ⇄ [potato] ⇄ [dragon fruit] ⇄ Tail
   (sentinel)  most recent                         least recent   (sentinel)
   Check("tree"): unlink [tree], relink after Head.   Add past SIZE 5: unlink Tail.Left.
```

---

## 3. Code, reconstructed

Reconstructed from the narration; names and control flow follow what he typed. `// inferred` marks
what he did not say. Go 1.22+ syntax.

```go
package main

import "fmt"

const SIZE = 5

type Node struct {
	Val   string
	Left  *Node
	Right *Node
}

type Queue struct {
	Head   *Node // sentinel
	Tail   *Node // sentinel
	Length int
}

type Hash map[string]*Node

type Cache struct {
	Queue Queue
	Hash  Hash
}

func NewCache() Cache { return Cache{Queue: NewQueue(), Hash: Hash{}} }

func NewQueue() Queue {
	head, tail := &Node{}, &Node{}
	head.Right = tail
	tail.Left = head
	return Queue{Head: head, Tail: tail}
}

func (c *Cache) Check(str string) {
	node := &Node{}
	if val, ok := c.Hash[str]; ok {
		node = c.Remove(val)
	} else {
		node = &Node{Val: str}
	}
	c.Add(node)
	c.Hash[str] = node
}

func (c *Cache) Remove(n *Node) *Node {
	fmt.Printf("remove: %s\n", n.Val)
	left, right := n.Left, n.Right
	right.Left = left
	left.Right = right
	c.Queue.Length--
	delete(c.Hash, n.Val)
	return n
}

func (c *Cache) Add(n *Node) {
	fmt.Printf("add: %s\n", n.Val)
	tmp := c.Queue.Head.Right
	c.Queue.Head.Right = n
	n.Left = c.Queue.Head
	n.Right = tmp
	tmp.Left = n
	c.Queue.Length++
	if c.Queue.Length > SIZE {
		c.Remove(c.Queue.Tail.Left)
	}
}

func (c *Cache) Display() { c.Queue.Display() }

func (q *Queue) Display() {
	node := q.Head.Right
	fmt.Printf("%d - [", q.Length)
	for i := 0; i < q.Length; i++ {
		fmt.Printf("{%s}", node.Val)
		if i < q.Length-1 {
			fmt.Printf("<-->")
		}
		node = node.Right
	}
	fmt.Println("]")
}

func main() {
	fmt.Println("START CACHE")
	cache := NewCache()
	for _, word := range []string{"parrot", "avocado", "dragon fruit", "tree", "potato", "tomato", "tree", "dog"} {
		cache.Check(word)
		cache.Display()
	}
}
```

---

## 4. Claims he makes, checked

| Claim (paraphrased) | Verdict | Why / reference |
|---|---|---|
| A "true LRU" moves a re-used item to the head, keeps order, adds at the head and evicts at the tail. | **true** | That is exact LRU; with a map to the node every step is O(1) (`container/list` documents the same list operations as O(1) `MoveToFront`, `Remove`, `PushFront`). |
| Unlinking a node ("nobody has the address of it any more") deletes it from memory. | **true, with a caveat** | The GC collects it once unreachable, but the removed node still points at its old neighbours; harmless here because `Check` relinks or drops it at once. `container/list` clears `next`/`prev` on `Remove` for exactly this reason. |
| Sentinel head and tail make an empty queue "head and tail pointing at each other". | **true** | The sentinels remove every nil check from Add and Remove; `container/list` uses one sentinel root the same way. |
| The hash map makes "does it exist" fast. | **true** | One map lookup instead of a list walk; it is what makes Check O(1). |
| Deletion "mostly" happens at the tail. | **true for eviction, not for use** | Every hit also unlinks from the middle — on a read-heavy cache most list writes are hits, which is the cost section 6 is about. |

---

## 5. Defects and gaps in the video code

1. **Not safe for concurrent use**: `Check` writes the map and four pointers; two goroutines corrupt the list or crash on a concurrent map write. A real cache needs a lock — and because a *read* moves a node, a write lock on every read (section 6).
2. **No separate value**: the key is the value; a cache stores `key → value`.
3. **No TTL**: entries only leave by eviction.
4. **No read API**: `Check` is read-and-insert; a miss cannot be told apart from an insert.
5. **`fmt.Printf` in the data path** (`add:` / `remove:`): I/O on every operation.
6. **`node := &Node{}` in `Check` is always overwritten** — a wasted allocation.
7. **Size is a package constant** (`SIZE = 5`), not a parameter.
8. **No tests**, no benchmark, no `-race` run.

---

## 6. What red_lite_cache takes from it, what it replaces

The reader's local map (`reader/local.go`) is read on every backendAdvo request from many goroutines
and written by one read loop. D-008 rejected an exact LRU in 2026-10 because "a write lock on every
Get hit"; D-043 (2026-11-10) picks sampled LRU, the audit's option (a).

**Takes**

- The goal: on a full map, evict the least recently used entry, not an arbitrary one.
- A map for O(1) lookup, and recording a use on every hit.
- Expired entries go first (his cache has no TTL; ours prefers an expired entry in the sample).

**Replaces — and why the linked list was not used**

- **A list move on every hit is a write.** Moving a node to the head changes 6 pointers shared by
  every reader, so every `Get` would take the write lock and parallel Gets would serialise. The
  reader's Get takes only the read lock and records the read as an atomic store of a millisecond
  timestamp into the key's slot, at most once per 100 ms per key (`accessResolution`; Redis uses
  1 s, which inverted LRU here when the map turned over within a second). Measured, interleaved
  with HEAD: parallel local Get at 1 000 / 10 000 / 100 000 entries 147.1 / 150.0 / 148.0 ns vs
  HEAD 154.5 / 156.7 / 147.3 ns (`doc/reader_optimize_2026-11-10/out/bench_after_interleaved.txt`,
  `bench_head_interleaved.txt`).
- **Exact order → a sample of 5.** Eviction draws 5 uniformly random slots and evicts the first
  expired one, else the least recently read (Redis `maxmemory-samples` 5). Not exact, but measured
  in `TestEvictionKeepsHotKeys`: 100 of 100 keys read between inserts survive 10× the cap in new
  keys, where the old arbitrary eviction kept 0 of 100.
- **Two pointers and a node per entry → a 32-byte slot in one array.** A list costs a heap node per
  entry and pointers the GC must trace; the slot array is one allocation, and swap-remove keeps it
  dense.
- **The draw must be uniform.** Go map iteration was tried as the sampler and rejected: a range
  starts at a random slot but returns the next full one, favouring entries next to holes — exactly
  the hot keys that survive while their neighbours are evicted. One 5-entry range window kept 90 of
  100 hot keys; five 1-entry ranges 96–99; uniform draws 100 (probe 2026-11-10, report S3).
- **`container/list` was not used either**: same write-on-read, plus `interface{}` values (an
  allocation and a type assertion per element).

---

## 7. Sources

- `container/list` — https://pkg.go.dev/container/list (doubly linked list; `MoveToFront`, `Remove` clear links).
- Redis key eviction, approximated LRU and `maxmemory-samples` — https://redis.io/docs/latest/develop/reference/eviction/
- `sync.RWMutex` (readers share the lock; one writer excludes all) — https://pkg.go.dev/sync#RWMutex
- `sync/atomic` — https://pkg.go.dev/sync/atomic
- This repo: D-008, D-043 (`doc/DECISIONS.md`); `doc/audit_2026-10-29.md` §4.3; `doc/reader_optimize_2026-11-10/report.md` § S3.
