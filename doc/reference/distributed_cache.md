# Distributed cache (Anthony GG) — distilled and checked

_Raw source: `examples/distributed_cache_in_golang.md`. Distilled 2026-10-29. Go version on stream:
~1.20 (2023). **Starting idea, not the answer** — every verdict below was checked against Go 1.27
docs and current issues; `.claude/rules/backend/*.md` win where they disagree. Cite as
`doc/reference/distributed_cache.md § <n>`._

**Why it matters here.** This is the only source that builds the same *shape* as advo_cache —
one process, two roles, leader forwards writes to followers over a persistent socket — from scratch,
on camera, including the mistakes. Sections 4 and 5 are a checklist of what `internal/ws`,
`internal/replica` and `reader/` must not repeat; section 6 maps each to this project's decision.


## 1. Source

| | |
|---|---|
| Title | Let's Build A Distributed Cache In Golang (livestream) |
| Author | Anthony GG (anthdm), 2023 |
| Language / module | Go, module `ggcache`, packages `main` and `cache` |
| Built on stream | An in-memory key/value cache behind a raw TCP listener with a hand-rolled space-separated text protocol. `SET key value ttl` and `GET key` are implemented end to end; `HAS` and `DELETE` exist only on the `Cacher` interface and `Cache` struct, never as wire commands. One process is the leader (`--leaderaddr` empty); any process started with `--leaderaddr` dials the leader and becomes a follower. The leader forwards every accepted `SET` to every connection in its `followers` map. |
| Explicitly out of scope (his words, cleaned up) | *Consensus / leader election:* "in a distributed system you need some kind of consensus to determine who is the leader ... we're not gonna do this right now ... if your leader gets wrecked you need a new leader and who's going to determine that, the consensus ... Raft, Paxos, whatever." Leader is a hard-coded CLI flag instead. *Persistence:* "Persistence? No. It's going to be all in memory. Keep it simple." *Reads from followers / out-of-sync tracking / per-forward deadlines:* described as ideas ("if we time out on follower A we flag that follower as out of sync") but never implemented; `context.TODO()` is passed and ignored. *JOIN command:* he realises twice that he needs one ("we need a join command for sure") and ships a hack instead. *Tests:* "are we going to test this? probably not." No tests were written. |
| Framing | He repeatedly states this is unscripted exploration, "not a production-ready system", and that the TTL and forwarding code is "very dirty, don't do this at home". Read the code below as a sketch of the shape, not as a reference implementation. |

---

## 2. Architecture as built

- **One process type, two roles.** `ServerOpts.IsLeader` is derived from `len(leaderAddr) == 0`. Both roles run the same TCP accept loop and the same command handler.
- **Follower join.** On `Start()`, a non-leader spawns a goroutine that `net.Dial`s the leader's listen address and runs the ordinary `handleConn` read loop on that connection. There is no handshake; the follower simply reads whatever bytes the leader writes and parses them as commands.
- **Leader's view of followers.** `Server.followers map[net.Conn]struct{}`. The stream-end hack registers **every** accepted connection as a follower when `IsLeader` is true, so ordinary clients are in the map too and receive forwarded `SET`s back.
- **Writes.** Client sends `SET k v ttl` to the leader. Leader parses, applies to its local `Cache`, then (in a goroutine) iterates `followers` and writes `msg.ToBytes()` to each. Each follower's `handleConn` parses it as a `SET` and applies it locally — and, being a non-leader, does not forward further.
- **Reads.** `GET k` is answered from whichever node received it; the raw value bytes are written straight back on the connection. No read routing, no "ask a follower" path.
- **Errors.** Parse/handle errors are written to the client as the error string. Forward failures are logged and skipped (`continue`).
- **State on join.** None. A follower that joins after keys were set never receives them.

```
                 SET foo bar 2500          GET foo
   client ───────────────────────► leader ◄──────────── client
                                    │  cache.Set  (local map)
                                    │  go sendToFollowers(msg)
                        ┌───────────┼───────────┐
                        ▼           ▼           ▼
                    follower    follower    (the client conn itself,
                    :4000       :5000        because every accepted
                   handleConn  handleConn    conn is in `followers`)
                   cache.Set   cache.Set

   each follower on start:  net.Dial(leaderAddr) ──► leader accept loop
                             (no JOIN, no auth, no catch-up)
```

---

## 3. Code, reconstructed

Reconstructed from the narration. Names, types and control flow follow what he said he typed; anything not
stated is marked `// inferred`. Where the narration shows him writing a bug, the bug is kept and flagged
(`// BUG as narrated`) so section 5 has something concrete to point at. Go 1.22+ syntax.

### `cache/cacher.go`

```go
package cache

import "time"

// Cacher is the storage abstraction the server depends on.
type Cacher interface {
	Set(key, value []byte, ttl time.Duration) error
	Get(key []byte) ([]byte, error)
	Has(key []byte) bool
	Delete(key []byte) error
}
```

### `cache/cache.go`

```go
package cache

import (
	"fmt"
	"log"
	"sync"
	"time"
)

type Cache struct {
	lock sync.RWMutex
	data map[string][]byte
}

func New() *Cache {
	return &Cache{data: make(map[string][]byte)}
}

func (c *Cache) Set(key, value []byte, ttl time.Duration) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	keyStr := string(key)
	c.data[keyStr] = value
	log.Printf("SET %s to %s\n", keyStr, string(value))

	// He first wrote time.NewTicker(ttl); chat pointed out time.After.
	go func() {
		<-time.After(ttl)
		// BUG as narrated: he says "we're already locking here because we've got a Set",
		// i.e. he believes the Set's lock covers this delete. It does not — Set has
		// returned and unlocked long before the timer fires. Unsynchronised map write.
		delete(c.data, keyStr)
	}()
	return nil
}

func (c *Cache) Get(key []byte) ([]byte, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	keyStr := string(key)
	val, ok := c.data[keyStr]
	if !ok {
		return nil, fmt.Errorf("key (%s) not found", keyStr)
	}
	log.Printf("GET %s = %s\n", keyStr, string(val))
	return val, nil
}

func (c *Cache) Has(key []byte) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	_, ok := c.data[string(key)]
	return ok
}

func (c *Cache) Delete(key []byte) error {
	// He considered calling Has() here and correctly noted it would take the
	// read lock inside the write lock ("you're gonna have a lock and unlock,
	// it's problems"), so Delete does not check existence.
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.data, string(key))
	return nil
}
```

### `command.go` (package main)

```go
package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Command string

const (
	CMDSet Command = "SET"
	CMDGet Command = "GET"
	// No HAS / DEL wire commands were defined on stream.
)

// Message is the parsed form of one wire command. He debated per-command structs
// (MSGSet, MSGGet) because "most fields are unused for most commands", then kept
// one struct "for now".
type Message struct {
	Cmd   Command
	Key   []byte
	Value []byte
	TTL   time.Duration
}

func parseCommand(raw []byte) (*Message, error) {
	rawStr := string(raw)
	parts := strings.Split(rawStr, " ")
	if len(parts) < 2 { // inferred: he said "check if there is nothing at all"; the key index would panic otherwise
		return nil, errors.New("invalid protocol format")
	}

	msg := &Message{
		Cmd: Command(parts[0]),
		Key: []byte(parts[1]),
	}

	switch msg.Cmd {
	case CMDSet:
		if len(parts) != 4 {
			return nil, errors.New("invalid SET command")
		}
		msg.Value = []byte(parts[2])
		ttl, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, errors.New("invalid SET TTL")
		}
		msg.TTL = time.Duration(ttl) // nanoseconds; the test client sends 2500000000
	case CMDGet:
		// nothing else to parse
	}
	return msg, nil
}

// ToBytes re-serialises a message for forwarding to followers.
func (m *Message) ToBytes() []byte {
	switch m.Cmd {
	case CMDSet:
		// BUG as narrated: he wrote three %s verbs ("it's gonna be S S S"), was unsure
		// about the TTL ("I'm a little bit confused about the formatting") and the
		// follower then rejected the forwarded message with "invalid SET command"
		// (3 parts, parser requires 4). The stream ends while he is logging rawMsg
		// to find exactly this. // inferred: TTL omitted
		return []byte(fmt.Sprintf("%s %s %s", m.Cmd, m.Key, m.Value))
	case CMDGet:
		return []byte(fmt.Sprintf("%s %s", m.Cmd, m.Key))
	default:
		panic("unknown command") // he wished for a panicf
	}
}
```

### `server.go` (package main)

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net"

	"ggcache/cache"
)

type ServerOpts struct {
	ListenAddr string
	IsLeader   bool
	LeaderAddr string
}

type Server struct {
	ServerOpts

	followers map[net.Conn]struct{} // "an empty struct takes no space and a map is easier to delete from than a slice"
	cache     cache.Cacher
}

func NewServer(opts ServerOpts, c cache.Cacher) *Server {
	return &Server{
		ServerOpts: opts,
		cache:      c,
		followers:  make(map[net.Conn]struct{}), // he noted this should only be allocated on the leader
	}
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen error: %s", err)
	}

	if !s.IsLeader {
		go func() {
			conn, err := net.Dial("tcp", s.LeaderAddr)
			if err != nil {
				log.Fatal(err)
			}
			log.Println("connected with leader:", s.LeaderAddr)
			s.handleConn(conn) // the follower reads forwarded commands on this conn
		}()
	}

	log.Printf("server starting on port [%s]\n", s.ListenAddr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %s\n", err)
			continue // "if we break here nobody can connect anymore"
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	buf := make([]byte, 2048) // "how big? it depends. 2048."
	fmt.Println("connection made:", conn.RemoteAddr())

	// BUG as narrated ("this is so bad, don't do this at home"): every accepted
	// connection on the leader is treated as a follower, clients included.
	if s.IsLeader {
		s.followers[conn] = struct{}{} // unsynchronised map write from many goroutines
	}

	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("conn read error: %s\n", err)
			break
		}
		// BUG as narrated: buf is reused by the next Read while this goroutine
		// may still be parsing buf[:n].
		go s.handleCommand(conn, buf[:n])
	}
}

func (s *Server) handleCommand(conn net.Conn, rawCmd []byte) {
	msg, err := parseCommand(rawCmd)
	if err != nil {
		log.Println("failed to parse command:", err)
		conn.Write([]byte(err.Error()))
		return
	}

	log.Printf("received command %s\n", msg.Cmd)

	switch msg.Cmd {
	case CMDSet:
		err = s.handleSetCommand(conn, msg)
	case CMDGet:
		err = s.handleGetCommand(conn, msg)
	}

	if err != nil {
		log.Println("failed to handle command:", err)
		conn.Write([]byte(err.Error()))
	}
}

func (s *Server) handleSetCommand(conn net.Conn, msg *Message) error {
	if err := s.cache.Set(msg.Key, msg.Value, msg.TTL); err != nil {
		return err
	}
	go s.sendToFollowers(context.TODO(), msg) // ctx is a placeholder for a future deadline
	return nil
}

func (s *Server) handleGetCommand(conn net.Conn, msg *Message) error {
	val, err := s.cache.Get(msg.Key)
	if err != nil {
		return err
	}
	_, err = conn.Write(val) // raw value bytes, no framing, no status
	return err
}

func (s *Server) sendToFollowers(ctx context.Context, msg *Message) error {
	for conn := range s.followers { // he says out loud this should be mutex-protected
		rawMsg := msg.ToBytes()
		fmt.Println("forwarding", string(rawMsg), "to follower")
		if _, err := conn.Write(rawMsg); err != nil {
			log.Println("write to follower error:", err)
			continue
		}
	}
	return nil
}
```

### `main.go`

```go
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"ggcache/cache"
)

func main() {
	listenAddr := flag.String("listenaddr", ":3000", "listen address of the server")
	leaderAddr := flag.String("leaderaddr", "", "listen address of the leader")
	flag.Parse()

	opts := ServerOpts{
		ListenAddr: *listenAddr,
		IsLeader:   len(*leaderAddr) == 0,
		LeaderAddr: *leaderAddr,
	}

	// Throwaway test client he toggled with comments during the stream.
	go func() {
		time.Sleep(2 * time.Second)
		conn, err := net.Dial("tcp", ":3000")
		if err != nil {
			log.Fatal(err)
		}
		conn.Write([]byte("SET Foo Bar 2500000000"))
		time.Sleep(2 * time.Second)
		conn.Write([]byte("GET Foo"))
		buf := make([]byte, 1000)
		n, _ := conn.Read(buf)
		fmt.Println(string(buf[:n]))
	}()

	server := NewServer(opts, cache.New())
	server.Start()
}
```

Makefile targets: `build` (`go build -o bin/ggcache`), `run` (leader on `:3000`), `runfollower` (`--listenaddr :4000 --leaderaddr :3000`).

---

## 4. Claims he makes, checked

| Claim (paraphrased) | Verdict | Why / reference |
|---|---|---|
| A goroutine per key that waits on `time.After(ttl)` then deletes is a fine way to expire keys. | **wrong** (as written) / **outdated** (the leak part) | (a) As written it is a data race: the delete runs without the lock after `Set` returned (Go memory model: concurrent map write + read without synchronisation is undefined; `go test -race` flags it). (b) Cost is a goroutine (initial stack sized adaptively from Go 1.19, historically 2 KiB) plus a runtime timer per key for the whole TTL; a million keys with 1 h TTL is a million parked goroutines. (c) Before Go 1.23 the `time.After` timer could not be collected until it fired; Go 1.23 made unreferenced timers collectable (gated on `go.mod` go ≥ 1.23 / `GODEBUG=asynctimerchan`). (d) Overwriting a key does not cancel the old timer, so a later `SET` with a longer TTL is deleted by the earlier timer. The idiomatic design is a stored expiry timestamp checked on read plus a periodic sweeper, or `time.AfterFunc` with cancellation. Refs: Go memory model, Go 1.23 timer wiki, Go 1.19 release notes. |
| Switching from `time.NewTicker` to `time.After` (chat's correction) is better. | **true** | A ticker fires repeatedly and must be `Stop`ped or it is never collected (even after Go 1.23 only *unreferenced* tickers are collected, and his goroutine references it). `time.After` fires once. Still goroutine-per-key, see above. |
| `sync.RWMutex` + plain map is the right choice over `sync.Map` here. | **true** | `sync.Map` docs: optimised for (1) write-once-read-many keys and (2) goroutines touching disjoint key sets; "most code should use a plain Go map instead, with separate locking." A cache with overwrites and TTL deletes is neither case. Caveat: `RWMutex` read locks contend on one shared reader counter and scale poorly past a handful of cores (golang/go#17973, open since 2016); sharding the map by key hash is the usual fix, not `sync.Map`. |
| Calling `Has()` from inside `Delete()` would be a locking problem. | **true** | `RWMutex` is not reentrant: "a blocked Lock call excludes new readers ... this prohibits recursive read-locking" (sync docs). Taking `RLock` while holding `Lock` deadlocks outright. |
| Raw TCP with our own protocol is faster than HTTP+JSON; "Redis is super fast" because of this. | **partially** | Directionally true: a line protocol avoids HTTP header parsing and reflection-based `encoding/json`. But (a) his protocol does `string(buf)` + `strings.Split` + `fmt.Sprintf` per message, which allocates several times per command — not obviously faster than a tuned HTTP/1.1 path; (b) Redis's speed comes from a single-threaded event loop, pipelining and data structures, with RESP being length-prefixed so it never scans for delimiters the way `Split` does. He hedges himself ("not quite sure, I think it is"). Refs: RESP spec, encoding/json docs. |
| "JSON is super, super slow." | **partially / overstated** | `encoding/json` v1 is reflection-driven and allocation-heavy relative to binary codecs, but "slow" is relative to the workload; Go 1.25 shipped `encoding/json/v2` as an experiment specifically to close much of that gap. For small cache messages the dominant cost is syscalls and locking, not JSON. |
| "Do we need this interface? Probably not, but it's good practice." | **partially** | Go guidance is the opposite of "always": define interfaces at the consumer, keep them small, and do not add one before there is a second implementation or a test double (Go code review comments "Interfaces"; proverb "the bigger the interface, the weaker the abstraction"). A 4-method `Cacher` used by exactly one server is premature; a `Cacher` that lets tests inject a fake is justified. |
| Using bytes with `iota` for the command tag "would be more efficient / save space" than a `string`. | **true, but not what he built** | A one-byte opcode plus length-prefixed fields (`encoding/binary`) is smaller and parses without scanning. He acknowledged it and kept the text form "for now"; the text form also means a space in a key or value silently corrupts parsing. |
| `type Command string` is an "alias" so the wire word can change independently. | **partially** | It is a *defined type*, not an alias (`type X = string` is the alias form). The point stands: the constant's value can change in one place. |
| `struct{}` takes no space, and a `map[net.Conn]struct{}` is easier to delete from than a slice. | **true** | Spec, size guarantees: a struct with no fields has size zero. Map delete is O(1) vs slice search + shift. |
| "This goroutine will not leak; if the connection closes it will clean up." | **partially** | `handleConn` does exit on read error, so the per-connection goroutine is fine. But the per-key TTL goroutines, the orphaned `handleCommand` goroutines holding a reused buffer, and `sendToFollowers` blocked on a stalled follower `Write` (no deadline) can all outlive the connection. |
| On `Accept` error, `continue` rather than `break` so the loop keeps serving. | **partially** | Right for transient errors, wrong for permanent ones (listener closed, fd exhaustion): it becomes a hot loop logging at full speed. `net/http.Server.Serve` sleeps with backoff on `net.Error` and returns on `ErrServerClosed`; `net.Error.Temporary` is deprecated since Go 1.18, so the modern check is "is the listener closed / is the context done". |
| Forward failures to a follower "don't matter" for now; later the leader will "flag that follower out of sync" with a context deadline. | **wrong as shipped** | A dropped forward is silent divergence: the leader has the key, the follower does not, and a `GET` to that follower returns "not found". Nothing records it. The deadline/out-of-sync bookkeeping was never written; `context.TODO()` is unused. |
| String keys instead of a custom key representation "put stress on the GC". | **true, with nuance** | `string(key)` allocates per call, and since Go 1.5 the collector skips scanning maps whose key and value types contain no pointers (golang/go#9477); `map[string][]byte` contains pointers in both, so a large map is scanned every cycle. This is the motivation behind bigcache/freecache-style `map[uint64]uint32` + byte-arena designs. |
| Raft is used by Consul to pick a leader; Consul, etcd, Badger, CockroachDB are Go, Redis is C. | **true** | Consul's consensus docs: Raft via `hashicorp/raft`; etcd also uses Raft. The language claims match the public repositories. |
| Gin is "bad, slow, reflection". | **wrong / outdated** | Gin's router is httprouter's radix tree, "zero allocation, no reflection" per its own docs and it tops most Go router benchmarks. Disliking its API is a preference; the performance claim is false. |
| `print`/`println` work without importing `fmt`. | **true, with a caveat** | They are spec'd under "Bootstrapping": implementation-provided, write to stderr, and "not guaranteed to stay in the language". Use `fmt`/`log` in real code. |

---

## 5. Defects and gaps in the stream code

Each one is concrete in the reconstruction above.

1. **Reused read buffer handed to a goroutine** (`go s.handleCommand(conn, buf[:n])`): the next `conn.Read(buf)` overwrites bytes a still-running handler is parsing. Data race; corrupted keys/values under any load.
2. **TTL delete without the lock**: the timer goroutine mutates `c.data` unsynchronised. Concurrent map read/write is fatal in Go ("fatal error: concurrent map writes"), not just incorrect.
3. **Goroutine + timer per key** for the full TTL: unbounded memory for large keyspaces, and no way to cancel on overwrite or delete, so a re-`SET` is deleted early by the stale timer.
4. **TTL of 0 means "delete immediately"**, not "no expiry": `time.After(0)` fires at once. The protocol has no way to express a non-expiring key.
5. **No message framing**: TCP is a stream. Two `SET`s written back-to-back arrive as one `Read` ("SET a b 1SET c d 2"), one large `SET` arrives split across `Read`s, and any value over ~2 KiB is truncated. `bufio.Reader` + newline or length prefix is the minimum.
6. **Space-delimited text with no escaping**: keys or values containing a space, or an empty value, break parsing; binary values are impossible.
7. **Responses are unframed and untyped**: a successful `GET` writes raw value bytes, an error writes the error text, a `SET` writes nothing. A client cannot tell a value from an error or know when a reply ends.
8. **Every accepted conn on the leader is a "follower"**: clients receive forwarded `SET`s, and a client that sends a malformed message would be treated as a peer. There is no JOIN, no role, no auth.
9. **`followers` map mutated without synchronisation** from the accept goroutines and iterated from `sendToFollowers` goroutines. Race; also, closed conns are never removed, so the leader writes to dead sockets forever.
10. **Forward write has no deadline and no `ctx` check**: a follower that stops reading fills its socket buffer, then `conn.Write` blocks a goroutine per pending `SET` on the leader indefinitely. No backpressure, no timeout, `context.TODO()` never consulted.
11. **Forward is fire-and-forget after a local commit**: lost forwards are logged and dropped. Followers silently diverge; nothing tracks lag or triggers resync. Ordering is also not guaranteed because each `SET` spawns its own goroutine.
12. **No state transfer on join**: a follower dialling in after keys exist never receives them and will serve "not found" for them until they are re-set.
13. **Follower dial is one-shot with `log.Fatal`**: leader down at follower start = follower process exits; leader restart later = follower never reconnects (the read loop breaks and the goroutine ends).
14. **Accept loop hot-spins on permanent errors** (closed listener, EMFILE).
15. **No `context.Context` anywhere real**: no shutdown path, `Start()` never returns, listener and conns are never closed on signal, goroutines cannot be told to stop.
16. **Followers also run the accept loop as if they were leaders** — a client can `SET` directly on a follower and that write is never propagated anywhere, splitting the dataset.
17. **The forwarded `SET` wire format was broken at stream end**: `ToBytes` and `parseCommand` disagreed on field count, so forwarding never actually populated a follower on stream.
18. **`HAS` / `DELETE` unreachable over the wire**; deletes therefore never replicate even in principle.
19. **Error strings leak to the client as the response body** and are indistinguishable from values (see 7); error wrapping uses `%s`, not `%w`, so callers cannot `errors.Is`.
20. **No authentication, no TLS, no tenant separation**: anyone who can reach the port can read, write and, on the leader, become a replication target.
21. **Partial writes unhandled**: `conn.Write(val)` return count ignored; large values on a slow socket can be cut.
22. **No tests, no `-race` run, no benchmarks** despite several performance claims; the one test is a sleep-based client goroutine inside `main`.

---

## 6. What red_lite_cache takes from it, what it replaces

Context: red_lite_cache is a Go 1.27 multi-tenant WebSocket cache with an embeddable reader, leader/replica replication, distributed locks and Postgres write-behind. House rules already fixed: plain map + `RWMutex`; lazy expiry + one janitor, never goroutine-per-entry; every goroutine takes a `ctx`; typed protocol structs; additive wire changes only.

**Takes (the shape that survives)**

- A storage interface (`Set/Get/Has/Delete` with TTL) separated from transport, so the engine is testable without a socket. Keep it small and defined where it is consumed.
- Plain `map` guarded by `sync.RWMutex` as the store; `sync.Map` rejected for the same reason he gave. Shard by key hash if read-lock contention shows up in profiles (golang/go#17973).
- Byte-slice keys and values at the API edge, string keys in the map; accept the `string(key)` allocation and measure before optimising.
- Leader applies locally first, then replicates; followers apply replicated writes through the same handler path as client writes. One handler, two entry points.
- `map[conn]struct{}` as the follower registry shape — but guarded, with removal on close.
- A single-port design where joining is a command, not a second listener (the idea he had and did not implement).
- The explicit scope line: consensus/leader election and persistence are separate concerns from the cache core. red_lite_cache keeps the leader static per deployment and does write-behind to Postgres rather than implementing a log.

**Replaces**

- Goroutine-per-key `time.After` expiry → expiry timestamp stored with the entry, checked lazily on `Get`/`Has`, swept by one janitor goroutine that takes a `ctx`. TTL 0 means no expiry.
- Space-split text protocol over raw TCP → typed protocol structs on WebSocket frames (frame = message, so no hand-rolled framing), with opcode + versioned fields; additive changes only.
- Raw value / error-string replies → every request gets a typed reply with status, so a client never confuses a value with an error.
- "Every accepted conn is a follower" → explicit JOIN/handshake carrying role, tenant and credentials; followers live in a mutex-guarded registry and are removed on close.
- Fire-and-forget forwarding with `context.TODO()` → per-replica send queue with bounded depth, write deadlines, ordered delivery, and lag accounting; a replica that falls too far behind is marked stale and resynced.
- No catch-up on join → snapshot-then-stream on join so a replica is consistent before it serves reads.
- Followers accepting direct writes → replicas reject writes (or proxy them to the leader); the embeddable reader is read-only by construction.
- `log.Fatal` on dial failure, no reconnect → replica dial loop with backoff bound to `ctx`.
- Reused read buffer across goroutines → one reader goroutine per connection decodes a full typed message before handing it off; no shared buffers.
- Accept-loop `continue` forever → return on listener close / ctx done, backoff on transient errors.
- No auth, no tenants → per-connection tenant scoping and authentication at the handshake; key namespaces per tenant.
- Unlocked map mutation from timers and accept goroutines → all map access under the lock; CI runs `go test -race`.
- Sleep-based manual test in `main` → unit tests on the engine, protocol round-trip tests, and a leader/replica integration test that asserts convergence.

---

## 7. Sources

- Go memory model — https://go.dev/ref/mem
- Go race detector — https://go.dev/doc/articles/race_detector
- Go 1.23 timer channel changes (unreferenced timers collectable; `asynctimerchan`) — https://go.dev/wiki/Go123Timer and https://tip.golang.org/doc/go1.23
- Go 1.19 release notes (adaptive initial goroutine stack size) — https://go.dev/doc/go1.19
- `time.After`, `time.AfterFunc`, `time.NewTicker` — https://pkg.go.dev/time
- `sync.Map` and `sync.RWMutex` docs (use cases; no recursive read locking) — https://pkg.go.dev/sync@go1.25.0
- golang/go#17973 "sync: RWMutex scales poorly with CPU count" — https://golang.org/issue/17973
- golang/go#9477 GC pauses with large maps; Go 1.5 skips scanning pointer-free maps — https://golang.org/issue/9477
- Go spec: size and alignment guarantees (zero-size structs); Bootstrapping (`print`/`println`) — https://go.dev/ref/spec
- Go code review comments, "Interfaces" — https://go.dev/wiki/CodeReviewComments#interfaces
- Go proverbs ("The bigger the interface, the weaker the abstraction") — https://go-proverbs.github.io/
- `encoding/json` v2 announcement (Go 1.25 experiment) — https://go.dev/blog/jsonv2
- `encoding/binary` — https://pkg.go.dev/encoding/binary
- `bufio` (framing with `Reader`/`Scanner`) — https://pkg.go.dev/bufio
- `net.Error.Temporary` deprecation — https://pkg.go.dev/net#Error
- Redis serialization protocol (RESP) — https://redis.io/docs/latest/develop/reference/protocol-spec/
- Consul consensus protocol (Raft) — https://developer.hashicorp.com/consul/docs/v1.20.x/architecture/consensus
- Gin introduction (httprouter radix tree, zero allocation, no reflection) — https://gin-gonic.com/en/docs/introduction/
- bigcache design notes (GC and pointer-free maps) — https://github.com/allegro/bigcache
- Dave Cheney, "The empty struct" — https://dave.cheney.net/2014/03/25/the-empty-struct
