# One project, many sheets — feasibility (2026-11-11)

**Feasible: yes, with conditions.** It works today with no code change (option A), but only with
project-scoped sheet names and one connection, one password and one bcrypt check per sheet. The
better general feature is **namespaces inside a sheet** (option D): sign in once, one connection,
each table a first-class namespace with its own stats, TTL default and size quota. The consumer's
data does not split cleanly into tables (§2.3), so cross-table operations must stay cheap and
ordered.

Paths without a prefix are this repo. `backendAdvo/…` is
`/Users/devinanderson/Desktop/advo_production/advomedix-v2/backendAdvo/…` (read only). Numbers are
medians of 6 interleaved rounds (`out/medians.txt`, raw output in `out/round<N>_*.txt`).

---

## 1. What exists today

### 1.1 Projects, keys, sheets, admission

| Fact | Where |
|---|---|
| A project is created by the admin API with its first API key (`POST /admin/projects`). The key is stored as sha256 (D-020). | `internal/api/api.go:102`, `:149-187`; `internal/tenant/tenant.go:37-40` |
| A sheet is created inside a project by `POST /admin/projects/{id}/sheets`, by the admin or by that project's key. It needs a name and a password. | `internal/api/api.go:103`, `:189-229`; `internal/sheet/registry.go:143-160` |
| **Sheet names are unique per node, not per project.** The registry is `map[string]*Sheet`, and Postgres has `PRIMARY KEY (name)`. Two projects cannot both have a sheet called `pulse`. | `internal/sheet/registry.go:52`; `db/migration/000001_init.up.sql:58-59` |
| **Password mode:** the sheet password is checked (bcrypt), then the project's suspension. | `internal/ws/admit.go:37-58`; `internal/sheet/sheet.go:106-109` |
| **Key mode:** the sheet must exist, the API key must resolve to that sheet's project, and the sheet password must still match. So every open costs one key lookup and one bcrypt compare. | `internal/ws/admit.go:60-77`; `internal/tenant/tenant.go:113-145` |
| **Autocreate** happens in password mode only, is on by default only in development, and creates sheets owned by the default project. A revoked name is never autocreated again. | `internal/config/config.go:130`; `internal/sheet/registry.go:93-113`; `internal/ws/admit.go:38-40` |
| **No limit on sheets** per project or per node, and sheets are never evicted. The only size bound is `REDLITE_CACHE_MAX_KEYS_PER_SHEET`: one value for every sheet, unbounded by default. | `internal/sheet/registry.go:230-231`; D-042; `internal/config/config.go:129`; `internal/sheet/sheet.go:282` |

### 1.2 One reader connection

- **One sheet per connection.**
  - A second `open` on the same connection is refused with `already in sheet`; the router holds one `*sheet.Sheet` (`internal/ws/router.go:23`, `:133-135`).
  - `reader.Config` names one `Sheet` (`reader/reader.go:60`).
- **Several readers in one process work today with no code change.**
  - Each reader is independent: its own `Config`, connection, local map, replay queue and miss records (`reader/reader.go:126-151`, `:163-200`).
  - The probe ran 30 readers in one process (`BenchmarkFootprint`, `sheets=30`).

**Cost per reader**, measured as both sides of the connection in one process (`out/medians.txt`):

| | 1 reader | 30 readers |
|---|---|---|
| Goroutines | 5 (reader `run` + `readLoop`, `reader/reader.go:183`, `reader/conn.go:233`; server read loop + writer + ping, `internal/ws/conn.go:188-190`) | 150 |
| Heap, empty sheets | 0.044 MB | 1.31 MB |
| Heap, 10,000 keys in total (256-byte values) | 5.85 MB (614 B/entry) | 7.51 MB (787 B/entry, +28%) |
| Heap, 100,000 keys in total | 56.5 MB (593 B/entry) | 61.6 MB (646 B/entry, +9%) |

### 1.3 Per sheet vs per server

| Thing | Scope | Where |
|---|---|---|
| Write lock and order (apply + broadcast) | **per sheet** (`Sheet.mu`) | `internal/sheet/sheet.go:63`, `:267-310` |
| Store, its RWMutex, the 3-part index | **per sheet** (`cache.New()` per sheet) | `internal/sheet/sheet.go:60`, `:73`; `internal/cache/index.go:54-60` |
| `seq`, sweep log, R6 `base_seq` check | **per sheet** | `internal/sheet/sheet.go:61`, `:65`, `:280` |
| Locks (`lock`/`unlock`) | one manager per server, keyed by (sheet, key) | `internal/lock/lock.go:21`, `:39` |
| TTL policy (default and jitter) | **per server** (one `Options.Policy`) | `internal/sheet/registry.go:31-40`; `internal/cache/policy.go:10-35`; `internal/config/config.go:126-127` |
| Max keys | per server value, applied per sheet | `internal/config/config.go:129`; `internal/sheet/sheet.go:282` |
| Janitor | **one per process**, walks every sheet | `internal/sheet/janitor.go:10-37`; `registry.go:258-264` |
| Replication | **one stream per replica, all sheets** (frames carry `sheet`; join snapshot is one frame per sheet) | `internal/replica/leader.go:157-175`; `protocol/messages.go:176-200` |
| Persistence | one writer for all sheets; rows keyed `(project_id, sheet, key)` | `internal/persist/writer.go:250`; `db/migration/000001_init.up.sql:70` |
| Stats | per server only: the registry sums every sheet; there is no per-sheet stats endpoint | `internal/sheet/registry.go:232-255`; `protocol/messages.go:239-259` |
| Session | per connection, names one sheet | `internal/session/session.go:23-30` |
| Reader local map, MaxEntries, replay queue, miss records | **per reader** (one sheet) | `reader/reader.go:75-89`, `:133-148` |

### 1.4 Consumer-specific shapes inside the cache

1. **`patientSeg = "patient"`, the head index for `<root>:patient:<uuid>` (D-037).** This is the only hard-coded consumer name in the cache code (`internal/cache/index.go:14-15`, `:33-50`, `:57-59`). The same name appears in comments and Stats docs (`internal/cache/store.go:10-11`, `:174`; `reader/reader.go:424-425`).
2. **Segment separator `:`.** The first and last segment index assumes it (`internal/cache/index.go:17-31`). This is a widespread convention (Redis recommends `object-type:id`), not AdvoMedix-specific, but it is not configurable.
3. **v1 wire shapes kept for the first consumer:**
   - `__NULL__` placeholder and `set_null` (`protocol/messages.go:43-48`).
   - TTL in minutes (`:99-100`) and `lock_ttl` in seconds (`:104-105`).
   - `jitter` is accepted and ignored (`:101`).
   - Error strings are frozen because "backendAdvo may compare against" them (`:50-51`).
   
   These are general enough, but they exist for AdvoMedix.
4. **`reader/v1compat`** is advo_cache's API surface, kept for backendAdvo's cut-over (`reader/v1compat/v1compat.go:1-32`). It has no `APIKey` field, so a key-mode project cannot use it (`:55-66`).
5. **One default TTL for every key** (15 min, `internal/config/config.go:126`) matches AdvoMedix's most common TTL. The consumer sets 5, 15, 30 or 60 min per root on every set (survey §4).

---

## 2. The consumer's real shape (backendAdvo/cache, read only)

### 2.1 Facts that change the question

- **backendAdvo does not use red_lite_cache yet.**
  - It imports `github.com/CrimsonCoder42/advo_cache` (`backendAdvo/go.mod:6`, replace at `:86`).
  - It makes one reader per process (`backendAdvo/cmd/api/main.go:128-136`; `cache/store.go:127-138`, "ONE STORE PER PROCESS" at `:409`).
  - It has no project or API key config, only `LeaderAddr`, `BackupAddr`, `Sheet` and `Password` (`backendAdvo/util/config.go:20-23`).
- **There are about 59 key roots, not 28.**
  - 28 are profile-scoped (`<root>:patient:<uuid>`, built by `cache/keys.go:60-62`).
  - About 31 more are users, tenants, roles, catalogs, content and analytics.
- **Roots are not tables.**
  - One root can hold 2 or 3 tables: `pulse` + `heart_rate_buckets`; `episode`/`condition` + `medical_notes`; `immunization` + its catalog; `user` + 6 user tables.
  - One table can be spread over 9 roots: patient_goals, inferred.

### 2.2 Table → root → proposed sheet (condensed; roots are defined in `backendAdvo/cache/keys.go`, owners are the `cache/<table>.go` files)

| Group | Roots → proposed sheet (one per line where they differ) |
|---|---|
| Vitals (7) | `bp`→vitals_blood_pressure · `pulse`→vitals_pulse (+heart_rate_buckets) · `bs`→vitals_blood_sugar · `o2`→vitals_oxygen_saturation · `pf`→vitals_peak_flow · `rr`→vitals_respiration_rate · `temp`→vitals_temperature (owners `cache/blood_pressure.go:128,153`, `pulse.go:153,179`, … `vitals_generic.go:257-350`) |
| Clinical (12) | `symptoms`, `med`, `econtact`, `bloodtype`, `immunization` (split: immunizations / immunization_types), `measurement`, `family`, `episode` (+notes), `condition` (+notes), `allergy`, `events`, `tr` (test_results) |
| Infant and sleep (4) | `diaper`, `feeding`, `sleep`, `sleepsess` (record keys are outside the profile prefix: `<root>:record:<id>`, `keys.go:3472-3474`) |
| Nutrition and fitness (6) | `nutrition`, `fitplan`, `fitroutine`, `fitlog`, `fitactivity` |
| Identity and access (11) | `user` (could split into users / user_permissions / notifications / notification_prefs / user_prefs), `t` (tenants), `role`, `auth_invite`, `invite`, `assoc`, `patient`, `notif`, `notif_pref`, `user_prefs`, `admin` (views over 5 tables) |
| Catalogs (8) | `symptomref`, `fitcat`, `fittype`, `fitmuscle`, `nutritionfood`, `exercisetype`, `dosageunit`, `immunizationtype` |
| Content and analytics (11) | `blog`, `image_campaign(s)`, `campaign`, `newsletter`, `site`, `activity`, `growth`, `mrr`, `nps`, `referral`, `acquisition` |

So "a sheet per table" is about **55–60 sheets** for AdvoMedix. The measurements below use 30, so
the connection and goroutine costs roughly double for the real count.

### 2.3 Operations that span roots (each would span sheets)

| Operation | Roots / sheets touched | Kind | Failure handling |
|---|---|---|---|
| `invalidateEpisodeAttachedTypes` (`cache/medical_episode.go:93-162`) | 10: 7 vitals + tr + symptoms + med | 10 prefix sweeps, each repeated 2 s later (`store.go:300-306`) | logged, queued for replay, never returned |
| `MedicalDeleteMedicalEpisode` (`medical_episode.go:281-312`) | 12 + events | 13 sweeps now, 12 delayed | same |
| `MedicalDeleteMedicalCondition` (`medical_condition.go:178-216`) | 13 + 10 × episodes | 13 + 10E sweeps | same |
| `Medical{Set}EpisodeLinks` / `ConditionLinks` (`medical_episode.go:441-484`, `medical_condition.go:307-337`) | episode, condition, the 10 above, tr, med, events | prefix | same; ID lookups after commit swallow errors |
| `invalidateDomainBoth` (`store.go:324-329`), sleep sessions (`sleep_sessions.go:58-72`) | 1–2 roots for 2 profiles | prefix + exact | exact delete Warn only |
| Events ↔ med writes (`events.go:192-236`, `scheduled_medications.go:221-323`) | events, med | prefix | same |
| `InvalidateAllEvents` (`events.go:250-253`) | the whole `events:` root | prefix | same |
| Fitness cascades (`fitness_routines.go:55-58`, `fitness_logs.go:100-103`, `fitness_plans.go:175-215`, `fitness_admin.go:50-261`) | up to 4 roots; admin also `fittype`, `fitmuscle`, `fitcat` and every profile's `fitlog` | prefix | same |
| `InvalidateNutritionCatalog` (`nutrition.go:54-58`) | `nutritionfood`, every `nutrition:patient:` | prefix | same |
| `SweepMetadataOverrideRoots` at every boot (`test_results.go:77-80`, `cmd/api/main.go:146`) | `tr:`, `symptoms:` (whole roots, for every reader) | prefix | same |
| `invalidateUser` (`users.go:99-117`) | `user:<uid>`, `t:<t>:user*`, aliases | prefix + exact | alias errors discarded |
| `invalidateTenantBilling` / `AuthUpdateTenantPlanAndFeatures` (`users.go:138-144`, `:428-438`) | t, admin, patient roster, **suffix `<t>:profile` across every user** | prefix + suffix | suffix Warn, not replayed |
| `invalidateRole` (`roles.go:92-106`) | **suffix `:permissions`, `:role_ids`, `:roles` across every user**, then role | suffix + prefix | Warn, loop continues |
| `invalidatePermissionSets` (`permissions.go:131-135`) | suffix `:permissions` (user root) | suffix | Warn |
| `AdminSyncTenantPlanRole` (`roles.go:240-257`) | user, admin | exact loop | returns at first error and skips the rest |
| Patient and association writes (`same_tenant.go:72-176`, `admin_tenants_users.go:94-434`) | patient, assoc, t, admin | prefix + exact | Warn and return on DB read failure |
| `InvalidateAllAnalytics` (`store.go:583-605`) | 9 roots (`activity:` … `blog:analytics:`) | 9 prefix sweeps | Error log, continue, return nil |

**Keys that hold data from two tables:**
- `user:<uid>:<t>:profile` carries `tenants.plan`, which is why a tenant write sweeps a suffix across users (`users.go:417-423`).
- The roster list carries the tenant plan (`users.go:142`).
- `assoc:*:cross` carries patient names (`same_tenant.go:87`).
- `allergy:…:food_names` serves nutrition (`keys.go:2514-2520`).
- med timelines read events (`events.go:186-189`).
- Goals sit under 9 roots.

A strict per-table split would have to pick one home sheet for each such key and clear it from a
writer that lives in another table's code. That is the same problem as today, now spread across
sheets.

### 2.4 Entry counts and sizes per root

**Per-root counts and sizes: unknown.** `advomedix-v2/doc/load_limits_2026-11-10/REPORT.md` has
only totals:
- 27k–54k new keys in 60 s of cold reads (`:181-182`).
- About 400 new keys/s took RSS from 287 to 990 MB in 6 min (`:201-203`).
- About 5–6 KB per cached range response (`:243-244`).
- Two `DeletePrefix` calls per pulse write (`:234`).

Per-route value sizes are in `advomedix-v2/doc/memcached_switch_2026-11-11/INVENTORY.md:98-118`,
for example a blog list at 1.09 MB and a fitness-log list at 454 KB.

---

## 3. Options

Ordering in the table below: each sheet is ordered internally, by its own seq. In A and B,
different sheets are not ordered relative to each other; C and D keep one seq for every table.

| | **A. One reader per sheet (today)** | **B. One connection, many sheets** | **C. One sheet, per-root rules** | **D. Namespaces in a sheet (recommended)** |
|---|---|---|---|---|
| Wire protocol | none | yes: open names a project, frames carry `sheet`, new `join`/`leave` (additive; v1 frames unchanged) | none | none for v1. One additive admin route for namespace policy. |
| Server | none (but see condition 1) | router holds many sheets per conn; Join/Leave per sheet; session ↔ many sheets; admission once per conn | per-root TTL default, per-root max keys, per-root counts (the root buckets already hold them, `internal/cache/index.go:55`) | C, plus a general head index (§5) |
| Reader | none (consumer holds N readers) | `Sheet(name)` handles: a local map, replay queue, miss records and seq **per sheet** on one session | per-root hit/miss counters | `Namespace(name)` handle that prefixes keys, per-namespace stats and per-namespace quota inside the one local map |
| v1compat | unaffected; it still has no APIKey | unaffected (v1 open still single-sheet) | unaffected | unaffected |
| Conns / goroutines per instance at 30 sheets | 30 / 150 | 1 / 5 | 1 / 5 | 1 / 5 |
| Heap per instance (100k keys) | 61.6 MB (+9%; +1.3 MB at empty) | ≈ 1 reader + per-sheet structs | 56.5 MB | 56.5 MB + counters |
| sheet_state on connect and reconnect | 30 frames on 30 conns, decoded in parallel; **30 bcrypt checks** | 30 frames on 1 conn (serial); 1 auth | 1 frame | 1 frame |
| R1 reconnect, R3 miss when down, R10 | met per reader; 30 independent states (some tables up, some down) | met; one state | met | met |
| R2 replay, D-005 | met **per sheet**. An overflow `delete_prefix ""` wipes one table only (a gain). | met, but needs one queue per sheet and ordered replay across them | met (one queue) | met (one queue) |
| R6 `base_seq` | met: a key lives in one sheet, so its invalidations share its seq | met per sheet (one miss-seq set per sheet) | met | met |
| R4, R7, R8, R9 | met | met | met | met; quota adds per-namespace eviction |
| R11, R12 | **R11 needs project-scoped names. R12: 30 sheet passwords to manage.** | 1 credential | 1 | 1 |
| Cross-sheet ops (§2.3) | N acked round trips (about 55 µs each, can run in parallel); no order across sheets for other readers; partial failure per sheet, each replayed by its own reader; suffix clears (`:permissions`) hit one sheet only if the user root is one sheet | N ops on one conn: ordered for this reader, not atomic | unchanged (one seq) | unchanged (one seq) |
| Replication and persistence | unchanged (already multi-sheet) | unchanged | policy must replicate and persist | policy must replicate and persist |
| Per-table settings enabled | reader MaxEntries per table; nothing server side (TTL policy and max keys are per server) | same as A | TTL default, max keys, server counts | C, plus reader quota and hit/miss per table |
| Consumer must change | root→reader map; every cascade calls the right reader; 30–59 sheets × environments to create; passwords | cut over to a new reader API; cascades name the sheet | nothing beyond the cut-over; optional policy | wrap keys in `Namespace(root)` handles (mechanical); cascades unchanged |
| Work | 0 code; condition 1 below | about 20 files (protocol, wire.md, router, admit, session, sheet Join, reader session/local/replay, v1 paths, conformance), 40+ tests | about 6 files, 15 tests | about 10 files, 25 tests, 1 migration |

**Condition 1 for A** (also needed by B): sheet names are unique per node
(`registry.go:52`, `000001_init.up.sql:58`). A general multi-project server needs either
`(project, name)` as the identity, or a mandatory `<project>.<table>` naming rule.

**What Redis and memcached do** (official docs):
- **Redis:**
  - **Logical databases** (`SELECT`) are per connection and default to 16. The docs say "Use Redis databases to separate keys within the same application when needed. Don't use them to run multiple unrelated applications" (https://redis.io/docs/latest/commands/select/).
  - **Cluster:** Redis Cluster supports only db 0 (https://redis.io/docs/latest/operate/oss_and_stack/reference/cluster-spec/). Valkey 9 added multiple databases in cluster mode (https://valkey.io/topics/cluster-spec/).
  - **Limits:** `maxmemory` is global, with no per-db cap (https://raw.githubusercontent.com/redis/redis/unstable/redis.conf).
  - **Stats:** INFO keyspace counts keys, expires and avg_ttl per db (https://redis.io/docs/latest/commands/info/).
  - **Key prefixes are the namespace convention:** "object-type:id" (https://redis.io/docs/latest/develop/using-commands/keyspace/). ACLs restrict a user to `~prefix:*` (https://redis.io/docs/latest/operate/oss_and_stack/management/security/acl/).
  - **Client-side caching in BCAST mode:** one RESP3 connection subscribes to several `PREFIX`es and receives invalidations per prefix, in order with its replies (https://redis.io/docs/latest/develop/reference/client-side-caching/). That is the closest match to D: one connection, namespaces by prefix.
  - **Prefix delete** is not native: SCAN then DEL/UNLINK (https://redis.io/docs/latest/commands/scan/).
- **memcached:**
  - No namespaces. The wiki's trick is a versioned key prefix that is bumped to invalidate a namespace (https://github.com/memcached/memcached/wiki/ProgrammingTricks).
  - The proxy can route by key prefix to different pools (https://docs.memcached.org/features/proxy/).
  - Per-class stats and LRUs come from slab classes, not namespaces (https://github.com/memcached/memcached/wiki/UserInternals).

Both systems treat namespaces as prefixes over one connection. Redis's own separate-keyspace feature
(databases) is discouraged for anything but in-app separation, and it has no per-db limits.

### Recommendation: D, namespaces inside one sheet

**What D keeps from today:**
- One sign-in and one connection.
- One seq, so every cross-table cascade in §2.3 stays ordered and replays in order, and R6 needs no new state.
- The current measured speed of one-match prefix and suffix sweeps (the index makes them O(match)).

**What D adds, the parts of "a sheet per table" that matter to any project:**
- Per-table counts, hits and misses.
- A per-table default TTL.
- A per-table size quota, so one table cannot evict another table's hot keys.
- Per-table clear (`delete_prefix "<ns>:"`, already O(match)).

**The real alternative, B, lost on four counts:**
1. It is the largest change: wire, router, sessions and the reader's core all change.
2. One connection gives up the two gains A actually measured (§4): write parallelism across connections, and kick isolation. One send queue means one slow table kicks them all (D-014).
3. It still leaves cross-sheet clears unordered.
4. Its one benefit over D, separate server mutexes, measured as irrelevant end to end (§4: a server apply is about 2 µs against a 55 µs round trip).

**A stays available** for a project that wants hard isolation today, provided its sheet names are
project-prefixed.

---

## 4. Measurements

**Machine and method:**
- Apple M1 Pro, 10 cores, 16 GB, macOS (Darwin 25.6), Go 1.27.3 darwin/arm64. Server and readers ran in one process on loopback, with `integration.StartLeader` and harness config.
- 6 rounds, each one `go test -count=1` process, so `sheets=1` and `sheets=30` alternated (interleaved). Rounds started every 2 min 15 s, steady from 08:17 to 08:31.
- **Drift:** none seen. The min–max spread is within ±10% on every line except `Mixed writes/s` (2.9k–7.6k vs 8.4k–11.4k) and `GetParallel total=10000 sheets=30` (99–129 ns).
- **Keys:** the consumer's shapes (as `reader/audit_test.go` benchKeys), over 30 roots: 4 of 5 are `<root>:patient:<uuid>:<view>` and 1 of 5 is `<root>:record:<uuid>`.
- **Values:** 256 bytes each.

| Benchmark (median, n=6) | 1 sheet | 30 sheets (1 reader each) | Change |
|---|---|---|---|
| Parallel `Reader.Get`, 10k keys | 135.9 ns | 101.8 ns | **−25%** |
| Parallel `Reader.Get`, 100k keys | 145.1 ns | 110.9 ns | **−24%** |
| `Set` of a new key on a full map, round trip, 10k / 100k | 54.2 / 54.8 µs | 52.9 / 53.5 µs | −2% (noise) |
| One-match `DeletePrefix`, round trip, 10k / 100k | 53.9 / 55.7 µs | 55.1 / 56.8 µs | +2% (noise) |
| One-match `DeleteSuffix`, round trip, 10k / 100k | 52.0 / 54.4 µs | 53.6 / 55.3 µs | +2–3% (noise) |
| Mixed: 16 writers on root A, 16 readers on root B; Get latency | 189.5 ns | 139.0 ns (2 sheets) | **−27%** |
| Mixed: writes/s achieved meanwhile | 10,902 | 4,100 (2 sheets) | −62%, noisy; cause not isolated |
| 32 writers, 2 roots, round trip, ns per write (1/throughput) | 14.2 µs | 8.5 µs (2 sheets) | **1.67× throughput** |
| Server `sheet.Apply` set, no network, 10k / 100k | 1.94 / 2.27 µs | 1.99 / 2.45 µs | +2–8% |
| Server `sheet.Apply` one-match prefix delete, 10k / 100k | 3.42 / 12.0 µs | 3.41 / 12.3 µs | none |
| Server apply, 32 goroutines in parallel | 1,254 ns/op | 301 ns/op | **4.2× throughput** |
| Heap, both sides, empty / 10k / 100k | 0.04 / 5.85 / 56.5 MB | 1.31 / 7.51 / 61.6 MB | +1.3 MB / +28% / +9% |
| Heap per entry, 10k / 100k | 614 / 593 B | 787 / 646 B | +28% / +9% |
| Goroutines (both sides) | 5 | 150 | ×30 |
| Connect (all readers at once, 100k keys loaded), bcrypt cost 4 | 359 ms | 64 ms | **5.6× faster** |
| Connect, bcrypt cost 10 (server default) | 431 ms | 316 ms | 1.4× faster |
| Reconnect after every connection is cut (backoff fixed at 10 ms), bcrypt 4 | 390 ms | 73 ms | 5.3× faster |
| Reconnect, bcrypt 10 | 457 ms | 335 ms | 1.4× faster |

**How to read it:**
- **Faster with separate sheets:**
  - Parallel local Gets: 30 RWMutexes instead of one shared reader counter. That saves about 35 ns per hit, which matters only at millions of Gets per second.
  - Writes from many goroutines: each connection has its own reader write lock and its own server read loop, so throughput rises 1.67×.
  - Connect: 30 sheet_states are decoded in parallel.
- **Slower:**
  - 30 bcrypt compares per instance on every (re)connect. At cost 10 that eats about 250 ms of the connect gain. Across 3 instances that is 90 compares on the leader after every failover, on the order of 90 × ~80 ms ≈ 7 CPU-seconds. That figure is an estimate derived from the two connect rows, not measured.
  - +9–28% heap, 30× connections and goroutines.
- **Unchanged:**
  - One-match deletes and puts end to end. The round trip (about 55 µs) dominates, and the index already makes sweeps O(match) at any sheet size.
  - Server per-op cost.
- **Only more organized:** per-table counts and quotas. A gets the quotas only in the reader; D gets them on both sides.

The writes/s drop in `Mixed sheets=2` is not explained by the code paths. It is likely CPU
scheduling of 32 busy goroutines on 10 cores, so do not read it as a cost of splitting.

---

## 5. Should the "patient" head index become general?

**Yes. Make it nameless:** index the head `<seg1>:<seg2>:<seg3>` for every key with at least three
segments, instead of only `<root>:patient:<uuid>`.

- **Why:** a project-agnostic cache must not name a consumer's segment (`internal/cache/index.go:15`). The shapes the consumer sweeps by head already have this form: `<root>:patient:<pid>`, `assoc:user:<uid>`, `events:tenant:<t>`, `role:users:<roleID>`.
- **How:** `headOf` takes the first three segments instead of matching the literal `patient`.
  - The bucket structure stays `root → head → keys`.
  - `NewWithoutHeadIndex` remains the off switch.
  - An optional per-sheet `head_depth` (2 to 4, default 3) covers other layouts.
- **Gate (D-037 style):** before switching, rerun conformance B1 and the store benchmarks on real key shapes. A key like `blog:list:all:<l>:<o>` would add head buckets that are never swept by head, so bytes per entry must be measured.

---

## 6. What stops being true or gets harder under A (and partly B)

- **A cascade is no longer one ordered stream.** Another instance can see the episode sheet cleared before the vitals sheets, for about one round trip. Today those deletes share one seq.
- **Clears that were one call become many.**
  - `InvalidateAllAnalytics` becomes 9 ops on 9 readers.
  - The role and permission **suffix** clears stay one call only if every `user:*` key stays in one sheet. Splitting `user` into 5 sheets turns each into up to 5.
- **Partial failure is per sheet.** Each reader replays its own queue (R2 holds), but the consumer's "Warn and continue" now hides which tables are stale.
- **Connection count:** 30–59 per instance, 90–177 for 3 instances.
- **Credentials:** a password per sheet (R12). Reconnect storms cost one bcrypt per sheet.
- **R11:** sheet names collide across projects until names are project-scoped.
- **Readiness:** "connected" becomes 30 booleans, so `/health` must decide whether one table down counts as down.

## 7. What AdvoMedix would change (list only, no edits there)

1. **Cut over from advo_cache to red_lite_cache** (`go.mod:6`, `:86`). Use `reader` directly, not `v1compat`, if key mode is wanted: v1compat has no APIKey.
2. **Add `CACHE_PROJECT` / `CACHE_API_KEY` config** (`util/config.go:20-23`), and move the sheet password out of `app.env:20` and the deploy workflow (`:142`).
3. **Under D:** in `cache/store.go`, wrap the reader once per root: `ns := r.Namespace("pulse")`, with keys unchanged after the prefix. Optionally move the per-root TTL constants (`keys.go:2570-2744`) to namespace policy. Cascades stay as they are.
4. **Under A instead:**
   - A `root → *reader.Reader` map built at boot (30–59 readers).
   - Every `invalidate*` routed to the right reader, with `InvalidateAllAnalytics` and the cascades looping over readers.
   - A home sheet chosen for each mixed key (§2.3).
   - `/health` deciding what "connected" means.
   - 30–59 sheets × environments created and their passwords managed.
5. **Either way, fix what the survey surfaced:**
   - The blog read-throughs write `blog:id:*`, `blog:list:*:<l>:<o>` and `blog:count:*`, and no write path deletes them (`cache/blog_posts.go:20-170` vs `:83-137`, `:348-352`).
   - The second sweep after 2 s (`store.go:300-306`) can be dropped after cut-over, since R6 covers it.

## 8. Build plan for D, if Devin says go (each step ships and tests alone)

1. **Project-scoped sheet identity.**
   - Registry key `(project, name)`; migration `000003` changes `sheets` PK to `(project_id, name)`.
   - Key-mode open resolves the name inside the key's project; password mode keeps global names (v1).
   - Tests: two projects, same sheet name; replica snapshot; persistence reload.
   - Needed by A too.
2. **Server per-root counts.**
   - `GET /admin/sheets/{name}/roots` returns entries per root from the root buckets (`internal/cache/index.go:55`), counts only (D-040).
   - Tests: counts follow set, delete and expire.
3. **Reader `Namespace(name)` handle.**
   - Get, Set, Delete, DeletePrefix and DeleteSuffix with a `name:` prefix; `Clear()` = `DeletePrefix("name:")`; per-namespace hit and miss counters in Stats.
   - Additive API, no wire change.
   - Tests: prefixing, isolation, R2/R6 through a namespace.
4. **Reader per-namespace quota.**
   - `Namespace(name, MaxEntries)`: eviction samples within the namespace first (D-043 slots, grouped per namespace).
   - Tests: a hot small namespace survives a large one's churn.
5. **Server per-root policy.**
   - Default TTL and max keys per root, per sheet.
   - Set by the admin API, persisted (migration), carried in `replicate_sheet` / `snapshot_sheet` (additive fields), applied in `Policy.ExpiresAt` and the MaxKeys check (`internal/sheet/sheet.go:282-286`).
   - Tests: policy survives promotion and restart.
6. **General head index (§5)**, behind the benchmark gate. Tests: the existing index tests with non-`patient` shapes, and the randomized index-vs-scan test.
7. **Docs:** a D-044 entry, wire.md (admin route), and a consumer guide.

## 9. Measured, then moved

`testing/_probe/sheetpertable_2026-11-11_test.go` (all benchmarks above) was moved to
`_delete/testing/_probe/`. To re-run it, move it back and run
`go test -run='^$' -bench=. -count=1 ./testing/_probe/`.

## OPEN DECISIONS FOR DEVIN

1. Recommend option D (namespaces in a sheet) as the general feature? **yes / no**
2. Make sheet identity `(project, name)` (migration 000003), so two projects can each have a `pulse`? **yes / no**
3. In key mode, admit with the API key alone, dropping the per-sheet password and its bcrypt per open? **yes / no**
4. Replace the `patient` literal with a nameless 3-segment head index, after a D-037-style benchmark? **yes / no**
5. Add per-root default TTL and max keys on the server (replicated and persisted)? **yes / no**
6. Keep option A documented as the zero-code way to get hard per-table isolation? **yes / no**
7. Split AdvoMedix's `user` root into several namespaces (it makes the role and permission suffix clears span them)? **yes / no**
