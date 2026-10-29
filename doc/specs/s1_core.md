# S1 Core (internal/cache + internal/sheet) — Spec

**Status:** built
**Reference:** `advo_cache/cache/cache.go:18-153`, `advo_cache/sheet/sheet.go:14-148`, `advo_cache/sheet/manager.go:14-64`

## Goal

The single-node core the server (S2), persistence (S5) and replica (S4) build on: a per-sheet TTL store
with indexed prefix/suffix sweeps, and a sheet registry that applies each write once, in seq order,
and fans one encoded frame out to every collaborator, for backendAdvo's cache.

## Acceptance criteria

1. An expired key is a miss on `Get`/`Has`/`All` before any janitor pass (lazy expiry, D-003).
2. A re-set replaces the expiry: an earlier deadline never removes a refreshed value (K5).
3. TTL 0 with `DefaultTTL` 0 never expires; every non-zero TTL gets 0..`JitterPercent`% added, never more.
4. `DeletePrefix`/`DeleteSuffix` delete exactly the keys `strings.HasPrefix`/`HasSuffix` would, including keys
   with no `:`, `""` (every key), and a prefix that ends mid-segment (`use` matches `user:1`); the segment
   index stays consistent after every Set/Delete/sweep/Expire/Replace (R7).
5. An indexed `DeletePrefix` over one root of 100k keys in 50 roots is faster than the full scan (R7).
6. KEY loop: set by A → B receives `replicate{key,value,seq,expires_in_ms}`, A receives only its reply;
   delete → `invalidate{key,seq}`; after the TTL both miss and the janitor broadcasts one `invalidate{keys,seq}` (K3).
7. SWEEP loop: 3 keys under a prefix (or suffix) + 1 outside → 3 removed, 1 kept, one `invalidate{prefix|suffix,seq}`.
8. ORDER: concurrent writers → every collaborator and every observer sees strictly increasing seq; a joiner
   sees open reply, then `sheet_state` (data non-nil), then only broadcasts after its state seq (K10, D-014).
9. BACKPRESSURE: a collaborator whose `Send` fails is kicked and dropped; the others and the sheet are unaffected (D-014).
10. R6: a set with `base_seq` older than a logged set/delete* covering its key, or below the log floor, is refused
    with `ErrStaleWrite`, nothing applied, seq unchanged, reply still sent; a newer base or nil base applies.
11. K12: `Restore` keeps the given hash; a promoted replica accepts the leader's password and nothing else; a sheet with no hash refuses all.
12. K13: the last `Leave` keeps the sheet and its data.
13. K14/D-013: autocreate off → `ErrSheetNotFound`; on → `created=true`, Warn `Sheet: autocreated`, OpCreate
    with `DefaultProject` observed before any op on that sheet; concurrent opens create exactly one sheet.
14. Wrong password → `ErrBadPassword`; empty name/password → `ErrNameRequired`; full sheet → `ErrSheetFull`.
15. `RunJanitor` skips passes while `active()` is false and returns nil on `ctx.Done`.

## Surface

| Kind | Name | Package |
|---|---|---|
| type/func | `Entry` (lead), `Policy.ExpiresAt`, `Store`, `New`, `Get`, `Has`, `Set`, `Delete`, `DeletePrefix`, `DeleteSuffix`, `Expire`, `Len`, `All`, `Replace` | internal/cache |
| type/func | `Registry`, `NewRegistry`, `Options`, `AddObserver`, `Open`, `Create`, `Restore`, `Get`, `Sheets`, `Remove`, `Len`, `ExpireAll`, `KickAll`, `RunJanitor` | internal/sheet |
| type/func | `Sheet` (`Name`, `ProjectID`, `PasswordHash`, `Seq`, `Len`, `CheckPassword`, `Join`, `Leave`, `Get`, `Has`, `Apply`, `ApplyReplicated`, `Snapshot`, `Replace`), `Mutation`, `Result`, `Snapshot`, `DefaultProject`, `Err*` | internal/sheet |
| message (built here, typed in protocol/) | `replicate`, `invalidate` (key / keys / prefix / suffix), `sheet_state` | protocol |

## Value-source table

| Value | Source (field / config / computed from) | Decided |
|---|---|---|
| `Entry.Value` | `Mutation.Value` = `Request.Value` bytes, never decoded | ✅ D-016 |
| `Entry.ExpiresAt` | `Policy.ExpiresAt(now, Request.TTL minutes)`; `DefaultTTL` = `REDLITE_CACHE_TTL` | ✅ D-003 |
| jitter | uniform 0..`REDLITE_CACHE_JITTER_PERCENT`% of the TTL, `math/rand/v2` | ✅ brief, v1 cache.go:147-153 |
| `seq` | per-sheet counter, +1 per applied op/expiry pass, under the sheet mutex | ✅ D-014 |
| `expires_in_ms` | `Entry.Remaining(now)` in ms; omitted when no expiry | ✅ D-008 |
| `sheet_state.epoch` | `Join(epoch)` argument from the caller (app/replica) | ✅ D-012 |
| `invalidate.keys` | `Store.Expire(now)` in one janitor pass | ✅ D-003 |
| `Result.Err` stale | sweep log: `REDLITE_SWEEP_LOG_WINDOW`, `REDLITE_SWEEP_LOG_MAX`, `Mutation.BaseSeq` = `Request.base_seq` | ✅ R6 |
| `Result.Err` full | `REDLITE_CACHE_MAX_KEYS_PER_SHEET` vs `Store.Len()` | ✅ brief |
| password hash | `bcrypt.GenerateFromPassword(pw, REDLITE_BCRYPT_COST)`; `Restore` hash from persistence/leader | ✅ D-013, K12 |
| autocreated project | `DefaultProject` (all-zero uuid, migration 000001) | ✅ D-013 |

## Failure behavior

| Failure | What the caller sees | What the system does |
|---|---|---|
| Collaborator queue full | its connection is kicked | dropped from the sheet; others keep receiving (Warn `Sheet: collaborator kicked`) |
| Unknown sheet, autocreate off | `ErrSheetNotFound` | nothing created |
| Stale read-through set | `ErrStaleWrite` in the reply | nothing applied, seq unchanged |
| Sheet full | `ErrSheetFull` | nothing applied; overwrites still allowed |
| Replica (inactive) | — | janitor skips; leader's `replicate_expire` arrives via `ApplyReplicated` |
| Persist unavailable | — | not this slice; observers enqueue and return |

## Reuse

- `advo_cache/cache/cache.go:18-22,44-51,62-88,147-153` → `Store` (map+RWMutex, expiry in the entry, segment index, jitter as policy).
- `advo_cache/sheet/sheet.go:14-21,66-82,84-101,111-148` → `Sheet`, `CheckPassword`, `Join`/`Leave`, `fanoutLocked`.
- `advo_cache/sheet/manager.go:14-17,27-64` → `Registry`, `Open`.
- code-heim go_18_worker_pool (`doc/reference/codeheim_patterns.md` § Tier 2) → `RunJanitor`.
- `cache.Entry` (lead, `internal/cache/entry.go`), `sheet.Op/Observer/Collaborator` (lead, `internal/sheet/op.go`), `protocol.Replicate/Invalidate/SheetState/Ptr/NullValue`.

## Decisions

D-002, D-003, D-008, D-009, D-012 (epoch passthrough), D-013, D-014, D-016.

## Wire compatibility

Additive only, all fields already in `protocol/`: `seq`, `expires_in_ms`, `epoch` on frames; `invalidate.keys`;
`invalidate.prefix` sent as `""` for a whole-sheet flush (a v1 reader treats it as nothing to do).

## Out of scope

WebSocket transport, locks, replica stream, persistence writer, admin HTTP API (S2–S6).

## Tests

| Criterion | Test | Kind |
|---|---|---|
| 1 | `internal/cache/store_test.go` `TestLazyExpiry` | unit |
| 2 | `TestResetExtendsLife` | unit |
| 3 | `TestNoExpiry`, `TestPolicy` | unit |
| 4 | `TestPrefixSuffixCases`, `TestIndexRandomOps`, `TestReplaceCopies` | unit |
| 5 | `store_bench_test.go` `BenchmarkDeletePrefixIndexed` vs `…Naive` (`doc/bootstrap_2026-10-29/bench_s1.txt`) | bench |
| 6 | `internal/sheet/sheet_test.go` `TestKeyLoop`, `TestSetNull` | unit |
| 7 | `TestSweepLoop`, `TestDeletePrefixEmptyBroadcastsPrefix` | unit |
| 8 | `TestOrderConcurrentApply`, `TestJoinOrderUnderLoad`, `TestSheetStateEmpty`, `TestObserversSeeEveryOp` | unit (-race) |
| 9 | `TestBackpressureKicks`, `TestSenderReplyFullKicksSender` | unit |
| 10 | `TestStaleWrite`, `TestApplyReplicatedAndReplace` | unit |
| 11 | `TestK12RestoreKeepsHash` | unit |
| 12 | `TestK13LeaveKeepsSheet` | unit |
| 13 | `TestK14Autocreate`, `TestConcurrentAutocreateOneWinner` | unit (-race) |
| 14 | `TestWrongPassword`, `TestSheetFull` | unit |
| 15 | `TestRunJanitor`, `TestKickAll` | unit |
