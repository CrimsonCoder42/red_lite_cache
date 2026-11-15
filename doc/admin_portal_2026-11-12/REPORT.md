# REPORT — admin portal backend wave (S1–S5), 2026-11-12

Spec: `doc/specs/s11_admin_portal.md` (route table, criteria, parameters). Plan: `PLAN.md`. Curl walkthrough and
lock-down: `WALKTHROUGH.md`. Decisions D-051…D-055.

## Harness (all `-race`, Postgres = own compose project `redlite_admintest`, 127.0.0.1:15444)

| Run | Line | Note |
|---|---|---|
| baseline (Devin, last full with DB) | 406 \| 403 \| 1 \| 2 | `TestPersistLoop` jsonb, known |
| `testing/runs/latest.json` before the wave | 1 \| 1 \| 0 \| 0 | a compose-only run |
| run 1 | 441 \| 436 \| 3 \| 2, 0 DATA RACE | `TestPersistLoop` (known); `TestStatsCountsOnly` (test bug: untagged decode struct); `TestGRPCSlowReader` (internal/rpc, untouched — green on its one re-run, 1.47 s: load timing) |
| fix re-runs | internal/api + internal/adminauth 52 \| 52 \| 0 \| 0 | after the checker fixes |
| **final** | **444 \| 441 \| 1 \| 2, 0 DATA RACE** | only `TestPersistLoop`; skips = `TestComposeReaderRoundTrip`, `TestLoadKicks` (opt-in) |

Raw output: `out/harness_run1.txt`, `out/fix_rerun.txt`, `out/harness_final.txt`. `TestUnknownEmailSameReply`
moved from bcrypt cost 12 (71.5 s under -race) to 10 (15.7 s): both paths do one compare at the configured cost.

## New tests against the pristine copy (C)

`internal/api` admin tests and `internal/adminauth` tests do not compile on the pristine tree (no
`golang.org/x/time/rate` in go.sum, no `adminauth.Limits`, no `Options.Admin`, no `db/sqlc/admin`).
`testing/integration` `TestStatsCountsOnly` compiles there and fails: `GET /admin/stats: 404`. Every test needing
Postgres ran (none skipped). Portable (black-box HTTP, would run against another implementation): every
`internal/api` admin test and `TestStatsCountsOnly`; internals only: `TestLimiterMapBounded`,
`TestLimiterRetryAfter`, `TestTrustedProxyOnlyFromLoopback`, `TestPasswordRules`.

## Checker (fresh subagent, criteria + owned-paths diff)

| Criterion | Verdict |
|---|---|
| bootstrap once under 10 concurrent callers | PASS (also probed at the SQL function, bypassing the early check: 1 uuid, 9 NULL) |
| no route reachable by a lower role | PASS (every route through `guard`; a project key on 10 operator routes → 401) |
| unknown-email = wrong-password reply, timing | PASS (byte-identical; probe at cost 10: 684–702 ms vs 688–714 ms) |
| cookie caller cannot POST cross-origin | PASS (PATCH same-site / DELETE foreign Origin → 403) |
| no secret in logs or audit | PASS (every new logger and audit call read) |
| limiter map bounded, sweep owned | PASS; concern: a full map swept per new key → **fixed** (at most once a second) |
| bearer key drives old routes, bootstrap.sh, peers | PASS; concern: the per-IP bucket could 429 the peer's 25-page merge → **fixed** (admin key skips it, `TestAdminKeySkipsIPLimit`) |
| persistence off degrades only new routes | PASS |

Other findings, fixed: NUL in email/name → 500 (now 401/400, `TestPortalInputEdges`); bootstrap with an email a
bearer-created user holds → 410 with the latch open (now 409); IPv6 zone into `inet` (zone dropped, test case);
bootstrap 201 body missing timestamps (now the stored row). Not fixed (ASSUMPTIONS.md): login CSRF, a refused
cross-origin request still slides the session, Retry-After 1 s when the map is full.

## Route table

See the spec. Summary: viewer reads (projects, sessions, stats, audit, me); admin writes projects/keys/sessions;
super_admin users + sheet export; bootstrap = admin key only; login public; logout/password cookie only.
Rate: per IP 20/40 (admin key exempt), per user 10/20, sign-in 5/min per IP (bootstrap, login, password).
