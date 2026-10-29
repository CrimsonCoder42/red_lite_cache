# Bootstrap 2026-10-29 — work folder

The one-shot build from scaffold to a running, tested base (`.claude/prompts/bootstrap.md`). This
README is THE BRIEF: agents read it instead of re-orienting. Facts below were verified by the lead
with file:line on 2026-10-29. Decisions D-002…D-018 are in `doc/DECISIONS.md`; anything an agent
decides alone goes in its report and the lead logs it in `doc/ASSUMPTIONS.md`.

## Baseline

`bash testing/run.sh` before any change: `Total | Passed | Failed | Skipped : 0 | 0 | 0 | 0`.
After Wave 0: protocol + logger `4 | 4 | 0 | 0`. `REDLITE_TEST_DATABASE_URL` is **unset** on this
machine: persist tests must `t.Skip("SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)")`.

## Waves and owned paths

| Wave | Who | Owned paths (edit ONLY these) |
|---|---|---|
| 0 | lead | protocol/, internal/config, internal/logger, db/ (migration 000001, query, sqlc, store.go), sqlc.yaml, .air.toml, Makefile, Dockerfile, docker-compose.yml, testing/integration/harness.go, internal/cache/entry.go, internal/sheet/op.go, doc ledgers |
| 1 | A | internal/cache/** (except entry.go), internal/sheet/** (except op.go), doc/specs/s1_core.md |
| 1 | C | reader/**, doc/specs/s3_reader.md |
| 1 | D | internal/persist/**, doc/specs/s5_persist.md |
| 2 | B | internal/lock/**, internal/ws/**, internal/app/**, cmd/redlite/**, testing/integration/harness_server.go, testing/integration/{key,sweep,lock,edge}_test.go, doc/specs/s2_server.md |
| 2 | lead | internal/api/** (standalone, mounted in wave 4) |
| 3 | D | internal/replica/**, testing/integration/{replica,persist}_test.go, doc/specs/s4_replica.md, ONE marked section in internal/app/app.go (`// ── replica + persist wiring (wave 3, agent D)`) |
| 3 | C | testing/integration/{disconnect,race}_test.go |
| 4 | lead | internal/api mount, testing/integration/api_test.go, docs, full run, reviewer |

Shared-file order: `protocol/` is FROZEN for agents — a needed field goes in your report and the lead
adds it between waves. `internal/config/config.go` (already has every REDLITE_* below) and
`.env.example` are lead-only. `go.mod`: never run `go get` or `go mod tidy`; the seven pre-cleared
modules are already required. `.env.example` is Read-denied to agents; the proposed file is
`doc/bootstrap_2026-10-29/env.example.proposed`.

## Rules every agent follows (short form; `.claude/CLAUDE.md` is the long form)

- Origin comment on every borrowed shape: `// after advo_cache/<file>:<line>: <what changed>` or
  `// after code-heim <repo>: …`. advo_cache is at `/Users/devinanderson/Desktop/advo_production/advomedix-v2/advo_cache` (READ-ONLY; the hook blocks any write and any non-read command naming it).
- Logging: `logger.L` only (`internal/logger`), message `Module: event`, snake_case attrs, never a
  value/password/hash/key material. The reader takes `Config.Logger *slog.Logger` (nil → `slog.Default()`), never `internal/logger`.
- Every goroutine takes a ctx and exits on it; no goroutine per entry; no lock held across network IO;
  `wg.Go` (Go 1.25+); a recovered panic logs at Error with its module.
- Errors wrap with `%w` and the module: `fmt.Errorf("sheet: open %q: %w", name, err)`; sentinels exported by the owning package.
- `map` + `sync.RWMutex`; no `sync.Map`. No `map[string]any` as a message type (the hook rejects it, even in tests). No `os.Getenv` outside internal/config (tests use `config.TestDatabaseURL()`).
- Tests: stdlib only. Line 1 of every test file: `// Covers: <package or action>`. Run with
  `bash testing/run.sh <pkg>` (it runs -race). Paste its `Total | Passed | Failed | Skipped` line.
- Never bind :8081/:8082. Never run git writes, docker, migrate, psql, go get, go mod tidy, rm, python3, bash -c.
- Deletes: none expected. If one is needed, list it in the report; the lead moves it.
- Your report (≤40 lines): owned paths, test lines, every log line added (file:line | level | message),
  origin comments, assumptions you made, fields you need in protocol/, FOUND NOT FIXED.

## Verified facts the agents build on

- Wire format: `doc/protocol/wire.md` + `protocol/messages.go` (lead, frozen). ttl is MINUTES; lock_ttl SECONDS.
- v1 reader handshake (advo_cache/reader/reader.go:376-441): sends `open`, reads frames until one with
  `success:true` (an `error` → ErrAuthFailed), then waits for `type:"sheet_state"`. Replies carry no `type`;
  `handleMessage` (reader.go:484-524) switches on `type` and ignores replies.
- v1 server routing (advo_cache/ws/handler.go:173-216): only `open`/`register_replica` before a sheet;
  else `must open sheet first`. Sender excluded from broadcasts (sheet/sheet.go:111-148).
- v1 set: ttl minutes, 0 → config default (handler.go:284-306); jitter flag → `SetWithJitter` (cache.go:147-153). Here jitter is server POLICY: every TTL gets 0..REDLITE_CACHE_JITTER_PERCENT added; the flag is accepted and ignored.
- v1 lock: owner = connection, same owner re-lock refreshes TTL, non-owner unlock → `not lock owner`, missing → `lock not found`, disconnect releases all (lock/lock.go:53-153, handler.go:137-150).
- v1 get of a missing key → `{success:false,error:"key not found"}`; set_null stores `"__NULL__"` (cache.go:16,131).
- Consumer (backendAdvo/cache/store.go:99-226): calls `reader.New(Config{LeaderAddr,BackupAddr,Sheet,Password})`,
  `Get(key)`, `Has`, `Set(ctx,k,v)`, `SetWithTTL(ctx,k,v,ttlMinutes)`, `Delete(ctx,k)`, `DeletePrefix(ctx,p)`,
  `DeleteSuffix(ctx,s)`, `Close()`, `IsConnected()`, and `errors.Is(err, reader.ErrClosed)` (store.go:336).

## CONTRACTS — exact Go surfaces (build against these; a change goes through the lead)

### internal/cache (agent A) — `entry.go` is lead-owned and already exists
```go
type Entry struct { Value json.RawMessage; ExpiresAt time.Time } // zero ExpiresAt = no expiry; Expired(now), Remaining(now)
type Policy struct { DefaultTTL time.Duration; JitterPercent int }
func (p Policy) ExpiresAt(now time.Time, ttlMinutes int) time.Time // ttlMinutes<=0 → DefaultTTL; DefaultTTL 0 → zero time; + 0..JitterPercent% jitter
type Store struct{ /* map[string]Entry + RWMutex + prefix/suffix index */ }
func New() *Store
func (s *Store) Get(key string, now time.Time) (Entry, bool)  // lazy expiry: expired = miss (and may delete)
func (s *Store) Has(key string, now time.Time) bool
func (s *Store) Set(key string, e Entry) (isNew bool)
func (s *Store) Delete(key string) bool
func (s *Store) DeletePrefix(prefix string) int                  // "" = every key
func (s *Store) DeleteSuffix(suffix string) int
func (s *Store) Expire(now time.Time) []string                   // remove and return expired keys
func (s *Store) Len() int
func (s *Store) All(now time.Time) map[string]Entry             // live entries, copied (snapshot/sheet_state/export)
func (s *Store) Replace(entries map[string]Entry)                // replica snapshot / hydration
```
### internal/sheet (agent A) — `op.go` is lead-owned and already exists (Op, OpKind, Observer, Collaborator)
```go
var ErrSheetNotFound, ErrBadPassword, ErrSheetExists, ErrNameRequired, ErrStaleWrite, ErrSheetFull error
var DefaultProject = uuid.UUID{} // all-zero: the 'default' project seeded by migration 000001 (owns autocreated sheets)
type Options struct {
    Policy cache.Policy; MaxKeys int /*0 = unbounded*/; Autocreate bool; BcryptCost int
    SweepLogWindow time.Duration; SweepLogMax int; Now func() time.Time /*nil = time.Now*/
}
func NewRegistry(o Options) *Registry
func (r *Registry) AddObserver(o Observer)                       // before serving; Observe runs under the sheet mutex
func (r *Registry) Open(name, password string) (s *Sheet, created bool, err error) // bcrypt never under a registry-wide lock; autocreate → OpCreate + Warn "Sheet: autocreated"
func (r *Registry) Create(project uuid.UUID, name, password string) (*Sheet, error) // admin API; ErrSheetExists; emits OpCreate
func (r *Registry) Restore(project uuid.UUID, name string, passwordHash []byte) *Sheet // hydration/replica: create or update hash; emits NOTHING
func (r *Registry) Get(name string) (*Sheet, bool)
func (r *Registry) Sheets() []*Sheet                             // snapshot of the current list
func (r *Registry) Remove(name string)                           // replica only: a sheet absent from the leader's snapshot
func (r *Registry) Len() int
func (r *Registry) ExpireAll(now time.Time) int                  // one janitor pass over every sheet; returns keys expired
func RunJanitor(ctx context.Context, r *Registry, interval time.Duration, active func() bool) error // ONE per process; skips a pass when !active() (replica); returns nil on ctx.Done
func (r *Registry) KickAll(reason string)                         // step-down / shutdown: Kick every collaborator

type Mutation struct {
    Kind OpKind /*OpSet|OpDelete|OpDeletePrefix|OpDeleteSuffix*/; Key string; Value json.RawMessage
    TTLMinutes int; Prefix, Suffix string; BaseSeq *uint64
}
type Result struct { Seq uint64; Deleted int; ExpiresIn time.Duration /*0 = none*/; Err error /*ErrStaleWrite | ErrSheetFull*/ }
func (s *Sheet) Name() string; ProjectID() uuid.UUID; PasswordHash() []byte; Seq() uint64; Len() int
func (s *Sheet) CheckPassword(pw string) bool
// Join: under the sheet mutex, add c, c.Send(openReply(seq)), then c.Send(<sheet_state frame: data, expires_in_ms, seq, epoch>).
// Every broadcast after Join reaches c after its sheet_state (ORDER contract).
func (s *Sheet) Join(c Collaborator, epoch uint64, openReply func(seq uint64) []byte)
func (s *Sheet) Leave(c Collaborator) int                         // never deletes the sheet (K13)
func (s *Sheet) Get(key string) (cache.Entry, bool)              // lock-free w.r.t. the sheet mutex (store RLock only)
func (s *Sheet) Has(key string) bool
// Apply: under the sheet mutex — check BaseSeq against the sweep log (R6) and MaxKeys, apply to the store,
// seq++, enqueue ONE encoded broadcast frame (protocol.Replicate / protocol.Invalidate, with seq and
// expires_in_ms) to every collaborator except sender (Send false → Kick), then sender.Send(reply(res)),
// then every Observer.Observe(op). On Err: nothing applied, seq unchanged, reply still sent.
// sender may be nil (admin API / tests).
func (s *Sheet) Apply(sender Collaborator, m Mutation, reply func(Result) []byte) Result
func (s *Sheet) ApplyReplicated(op Op)                           // replica stream: ignore op.Seq <= Seq(); apply; seq = op.Seq; log invalidations; observers; broadcast to collaborators (none on a replica, harmless)
func (s *Sheet) Snapshot(fn func(Snapshot))                      // fn runs UNDER the sheet mutex (replica hub enqueues atomically w.r.t. ops)
func (s *Sheet) Replace(snap Snapshot)                           // replica: entries, seq, hash replaced
type Snapshot struct { Name string; ProjectID uuid.UUID; PasswordHash []byte; Seq uint64; Entries map[string]cache.Entry }
```
**Sweep log (R6).** Per sheet, a bounded log of (seq, op kind, key|prefix|suffix, time) for every set
and delete* (not expiry), pruned to `SweepLogWindow` and `SweepLogMax`; `floor` = highest seq pruned.
A Mutation with `BaseSeq != nil`: `*BaseSeq < floor` → ErrStaleWrite; any logged entry with seq >
*BaseSeq that covers the key → ErrStaleWrite. `BaseSeq == nil` → unconditional (v1).

### internal/persist (agent D, wave 1) — depends only on db/ and sheet.Op
```go
type Options struct { BatchSize int; FlushInterval time.Duration; QueueSize int; CleanupInterval time.Duration }
func NewWriter(store *db.Store, o Options, active func() bool) *Writer
func (w *Writer) Observe(op sheet.Op)          // sheet.Observer; non-blocking; !active() → ignored; full → drop + Warn "Persist: enqueue dropped" (rate-limited)
func (w *Writer) Run(ctx context.Context) error // the one writer goroutine; on ctx.Done flushes what it holds with its own 10s deadline, then returns nil
type SheetRow struct { ProjectID uuid.UUID; Name string; PasswordHash []byte }
type EntryRow struct { Key string; Entry cache.Entry }
func Hydrate(ctx context.Context, store *db.Store, load func(SheetRow, []EntryRow) error) (sheets, entries int, err error)
func RunCleaner(ctx context.Context, store *db.Store, interval time.Duration) error
func LoadEpoch(ctx context.Context, store *db.Store) (uint64, error)   // 0 when no row
func RaiseEpoch(ctx context.Context, store *db.Store, epoch uint64, instanceID string) (uint64, error)
```
OpCreate → `registry.UpsertSheet` (autocreated = project is DefaultProject). OpSet/Delete*/Expire → `entries.*`.
A batch is one transaction (`db.Store.ExecTx` or `db.Store.Batch`), collapsed per key to the last op,
retried once on the next tick then dropped with an Error log carrying the count.

### reader/ (agent C) — imports ONLY protocol + coder/websocket + stdlib
```go
var ErrNotConnected, ErrClosed, ErrAuthFailed, ErrStale /*set refused as stale (R6)*/, ErrTimeout error
type Config struct {
    LeaderAddr, BackupAddr, Sheet, Password string                // v1 fields, same names
    Context context.Context; Logger *slog.Logger                  // optional
    ConnectTimeout time.Duration   // New waits this long for the first connect (default 5s); then returns the reader anyway, still reconnecting (R10). ErrAuthFailed is returned.
    WriteTimeout time.Duration     // ack wait per write (default 5s)
    MaxEntries int                 // local map bound (default 100000) (R4)
    MaxPendingDeletes int          // queue bound while disconnected (default 10000); overflow → one delete_prefix "" (D-005)
    ReconnectMin, ReconnectMax time.Duration // backoff 100ms .. 30s, full jitter, forever (R1)
}
func New(cfg Config) (*Reader, error)
func (r *Reader) Get(key string, target any) bool   // decode once into target (R8); miss while disconnected (R3); miss for "__NULL__"; miss after local expiry (R4)
func (r *Reader) GetRaw(key string) (any, bool)     // v1 shape
func (r *Reader) Has(key string) bool               // false while disconnected
func (r *Reader) IsNull(key string) bool
func (r *Reader) Set(ctx context.Context, key string, value any) error
func (r *Reader) SetWithTTL(ctx context.Context, key string, value any, ttlMinutes int) error // waits for the ack (R9); applies locally when the ack is read (in read-loop order); sends base_seq (R6)
func (r *Reader) Delete(ctx context.Context, key string) error         // connected: send + await ack; disconnected or failed: apply locally, queue, return nil (R2)
func (r *Reader) DeletePrefix(ctx context.Context, prefix string) error
func (r *Reader) DeleteSuffix(ctx context.Context, suffix string) error
func (r *Reader) IsConnected() bool                 // true only after sheet_state is loaded AND queued deletes are acked
func (r *Reader) Close() error
```
R6 in the reader: on a Get/Has miss record `missSeq[key] = appliedSeq` (bounded map); `SetWithTTL` sends
`base_seq = missSeq[key]` if recorded, else the current applied seq; on reconnect every recorded value
becomes 0 (conservative). Applied seq = max of sheet_state.seq, every broadcast's seq, every ack's seq.
Epoch (D-012): remember the highest epoch from open replies; an open reply with a lower one → close,
log Warn, try the other address. Every write carries `epoch`.

### internal/ws + internal/lock + internal/app (agent B, wave 2) — contract fixed when wave 1 lands
`app.New(ctx, cfg) (*App, error)`; `(*App).Run(ctx, ln net.Listener) error` — one errgroup owning the
HTTP server, janitor, lock cleaner, and (wave 3) replica node + persist writer + cleaner; graceful
shutdown on ctx. Harness: `StartLeader(t, ...func(*config.Config)) *Server` with `Server{URL, WSURL, ReplicaURL, Addr, App, Stop()}`.
`ws.Conn`: the per-connection send queue + writer + ping, implementing sheet.Collaborator, reused by internal/replica.
