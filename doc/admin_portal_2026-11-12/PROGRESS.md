# PROGRESS — admin portal backend wave (S1–S5)

- [x] 0 pristine copy in scratchpad `pristine/` (HEAD 1b96495); baseline latest.json = compose-only 1|1|0|0, last full with DB 406|403|1|2 (Devin)
- [x] 0 spec doc/specs/s11_admin_portal.md
- [x] 1 S1 SQL: 000003_admin.proposed.sql + 000003_admin.down.proposed.sql + db/query/admin/admin.sql (17 queries)
- [x] 1a Makefile (Devin's request at the checkpoint): DB_URL/TEST_DB_URL from REDLITE_PG_PASSWORD + REDLITE_HOST_PG_PORT; createdb/dropdb/migrateup/migratedown/test-persist recipes silent (@)
- [x] 1b SQLC CHECKPOINT: migration applied, sqlc.yaml admin block, sqlc fixed twice (@x * interval → sqlc.arg; nullable casts → COALESCE/flags), symbols verified
- [x] 2 S2 auth: internal/api/admin_auth.go, api.go guard + Register + authenticate, audit actor
- [x] 3 S3: internal/adminauth limiter/clientip, app.go timeouts + limiter Run, sha256 key compare
- [x] 4 S4: internal/api/admin_users.go, admin_store.go
- [x] 5 S5: admin_reads.go, persist/admin_purge.go + wire_replica.go call, config section, .env.example, compose
- [x] 6 tests: internal/api admin_*_test.go, internal/adminauth/adminauth_test.go, testing/integration/admin_portal_test.go
- [x] PHASE 2: run 1 441|436|3|2 → fixes → checker → fixes → final 444|441|1|2, docs, cleanup

# PROGRESS — admin portal frontend wave (S6–S8)

- [x] F1 backend: cross-origin check before credential resolution for every unsafe browser request (login CSRF; a refused request no longer slides the session) internal/api/admin_auth.go guard; prod bcrypt 12 = admin_auth.go:57 + adminauth.go:56
- [x] F2 portal/: scaffold, copied theme/ui/layout/modals (trimmed), icons as inline SVG, api client, auth context, ProtectedRoute, toast, /login /password /projects /projects/:id + show-once dialog; tsc + build clean
- [x] F3 serving: internal/api/portal.go + app.go mount; Dockerfile portal stage (image 26 → 26.5 MB, redlite:portalcheck); compose leader REDLITE_PORTAL_DIR=/portal; .dockerignore; make portal-dev
- [ ] F4 ⏸ STOP 1 — waiting for Devin's click-through
