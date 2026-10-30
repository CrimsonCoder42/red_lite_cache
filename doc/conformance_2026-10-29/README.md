# Conformance 2026-10-29 — work folder (THE BRIEF + progress)

Request: the backendAdvo consumer-conformance suite, `reader/v1compat`, the R7 index decision, and the
migration plan (D-032…D-037 in `doc/DECISIONS.md`). Agents read this instead of re-orienting.

## Baseline

`bash testing/run.sh` before any change: `Total | Passed | Failed | Skipped : 238 | 225 | 0 | 13`
(13 = persist, `REDLITE_TEST_DATABASE_URL` unset → `SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)`).

## Verified facts (read by the lead, 2026-10-29; backendAdvo = `/Users/devinanderson/Desktop/advo_production/advomedix-v2/backendAdvo`, READ-ONLY)

| Fact | Where |
|---|---|
| `cachedList`: `Get(key,&cached)` → hit return; miss → `load()` → error returned, NOT cached → `_ = SetWithTTL(ctx,key,points,ttlMinutes)` | `cache/shared.go:109-120` |
| `cachedOne`: same; a zero value that loaded is cached; an error returns `loaded, err` uncached | `cache/shared.go:146-157` |
| `invalidateDomain`: `sweepPrefix(domainPatientPrefix(root,pid))` then `time.AfterFunc(secondSweepDelay, sweepPrefix)` — the second sweep is G-01-16 (R6 workaround) | `cache/store.go:269-275` |
| `invalidateDomainBoth`: owner, then requester if `!= uuid.Nil && != owner` | `cache/store.go:293-298` |
| `Store.Get`: `linkReady()` gate, then `reader.Get`, then `json.Marshal` + `json.Unmarshal` into target (double pass) | `cache/store.go:166-178` |
| banner of advo_cache defects 1–5 (dropped deletes, no local TTL, expiry not broadcast, full-scan DeletePrefix, double JSON pass) | `cache/store.go:8-87` |
| DROPPED SWEEPS queue + `linkReady` + `replayDroppedSweeps` | `cache/store.go:340-520` |
| `domainPatientPrefix` = `"%s:patient:%s"` | `cache/keys.go:60-62` |
| `domainCursorKey` = `"%d:%s"` (nanos, uuid or `none`; absent cursor = `0`) | `cache/keys.go:81-102` |
| `domainFeedWindowKey` = `"%s:feed:%d:%d"` (start/end UnixNano) | `cache/keys.go:127-129` |
| TTLs (minutes): `TTLAnalytics=5` :2553, `TTLVitals=15` :2574, `TTLEmergencyContacts=30` :606, `TTLNutritionCatalog=60` :2442, `TTLTenant=60` :2707 | `cache/keys.go` |
| user keys: `UserRecordKey` `<UserPrefix>:<tenant>:record` :176, `UserProfileKey` `…:profile` :187, `UserPermissionsKey` `…<tenant>:permissions` (suffix const) :195 | `cache/keys.go` |
| 357 `func …Key(` builders (the request said 315) | `grep -c "^func .*Key(" cache/keys.go` |
| boot: `cache.NewStore(...)`; `if err != nil { Logger.Fatal("cannot connect to cache") }` | `cmd/api/main.go:112-120` |
| env: `CACHE_LEADER_ADDR`, `CACHE_BACKUP_ADDR`, `CACHE_SHEET`, `CACHE_PASSWORD` | `util/config.go:19-24` |
| **NOT FOUND**: `testing/new_backend/helpers.sh` (`assert_write_invalidates`), `vitals/blood_pressure.sh`, `infant/diaper.sh` do not exist under backendAdvo; `grep -rl assert_write_invalidates curl_tester curl_tests` finds nothing. backendAdvo's runnable harness on disk is `curl_tester/run.sh` + `curl_tester/phase-*.sh` (+ `curl_tests/*.sh`). C2 is modelled on the request's description of `assert_write_invalidates` | `ls backendAdvo/curl_tester` |

## Built by the lead (wave 0)

- `testing/conformance/consumerstore.go` — `Store{R *reader.Reader}`, `CachedOne[T]`, `CachedList[T]`, `(*Store).InvalidateDomain` (no second sweep; refuses empty root / nil profile with `ErrEmptyPrefix`), `InvalidateDomainBoth`, `DomainPatientPrefix`, `DomainFeedWindowKey`, `DomainCursorKey`, `EncodeCursor`, TTL consts, `FakeDB` (`Put`, `Load`, `Loads()`, `Fail(bool)`, `LoadHook func(profile)` to park a load mid-flight), `Row`. Public reader API only.
- Harness to reuse (do not edit): `testing/integration` exports `StartLeader(t, mods...)`, `StartReplica`, `TestConfig`, `NewReader(t, srv, sheet, mods...)` (password "pw", fast reconnect), `NewReaderAt`, `NewProxy` (`Cut`/`Block`/`Unblock`), `Dial`/`RawConn`, `UniqueSheet`, `Eventually`, `tnKeyMode`-style config mods are in `tenancy_api_test.go` (test file — copy the 5 lines, don't import). `srv.App.Projects()`, `App.Registry().Create(project, name, pw)`, `App.Tenants()`.

## Owned paths

| Agent | Owns | Delivers |
|---|---|---|
| A | `testing/conformance/**` EXCEPT `consumerstore.go` (lead) and `v1compat_test.go` (B) | `keycorpus.go` (≥40 shapes read from keys.go, builder lines cited), C1–C11, C13, edge cases |
| B | `reader/v1compat/**`, `internal/cache/**`, `testing/conformance/v1compat_test.go`, `testing/conformance/bench_test.go` | v1compat + C12; two-segment index + B1 (benchstat into `doc/conformance_2026-10-29/bench_b1.txt`) |

B1 needs the corpus: B reads `keycorpus.go` once A has written it — until then B benchmarks against the index with its own generator of the same documented shapes (`<root>:patient:<uuid>:<view>[:params]`) and switches when the file exists. Report which. Lead: docs, ledgers, this README.

## Rules (short)

Line 1 of every conformance file `// Covers: backendAdvo cache layer`. Origin comments `// after backendAdvo/cache/<file>:<line>: …` on every mirrored shape. 3 readers per test, load counts asserted. No sleeps as synchronisation: channels, `Eventually` with a reason, or `FakeDB.LoadHook`. No test passes by waiting for a TTL except C7, which tests the TTL. Never touch reader/ (except B's new `reader/v1compat/`), protocol/, internal/ (except B's internal/cache), go.mod, advomedix-v2. Run with `bash testing/run.sh testing/conformance` (run.sh maps a path to `./<path>/...`; only `integration` has a shorthand).

## Progress

| Unit | Agent | Status | Test line |
|---|---|---|---|
| consumerstore.go | lead | built | `go vet ./testing/conformance/` clean |
| reader/v1compat + C12 | B | built | `bash testing/run.sh internal/cache reader testing/conformance`: `112 \| 112 \| 0 \| 0` (TestC12V1CompatSurface, TestC12V1NewAgainstNothing, TestC12V1MaxReconnectAttemptsIgnored) |
| two-segment index + B1 | B | built, **KEPT** (D-037) | internal/cache `52 \| 52 \| 0 \| 0`; B1 on generated shapes: sweep 0.35×, miss 0.032×, new key 1.30×; **on 10 526 corpus keys (authoritative, reproduced by the checker): sweep 0.44×, miss 0.039×, new key 1.43×** (`bench_b1.txt`) |
| keycorpus.go + C1–C11, C13, edge cases | A | built | `bash testing/run.sh testing/conformance`: `60 \| 60 \| 0 \| 0`; `-count=3` C3/C8: ok 11.066s; 93 shapes / 27 roots |
