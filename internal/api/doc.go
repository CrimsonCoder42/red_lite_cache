// Package api is the HTTP management API (S6): the registry that lets a second project get its own
// sheet and API key without touching code (K14, R11, R12).
//
//	POST /admin/projects               admin key           → project + API key (SHA-256 at rest)
//	POST /admin/projects/{id}/sheets   admin or project key → a sheet readers can open
//	GET  /admin/sheets/{id}/export     admin or owner key   → every live entry (replaces `dump`)
//	POST /admin/projects/{id}/revoke   admin key           → keys revoked, sheets dropped
//
// Tenancy (D-026…D-030, admin key only; tenancy.go, sessions.go): GET /admin/projects[/{id}],
// POST /admin/projects/{id}/keys, DELETE /admin/keys/{id}, POST /admin/keys/{id}/rotate,
// POST /admin/projects/{id}/suspend|resume, GET /admin/sessions, GET /admin/sheets/{name}/sessions,
// DELETE /admin/sessions/{id}. The full table is doc/specs/s6_api.md.
//
// Admin portal (S11, admin_auth.go, admin_users.go, admin_reads.go): every route goes through one
// guard (rate limits, credential, role). "admin key" above now means an operator: the bearer admin
// key (super_admin) or a signed-in user (session cookie) with the role doc/specs/s11_admin_portal.md
// gives the route; export is super_admin or the owner key (D8). New: POST /admin/bootstrap,
// POST /admin/auth/login|logout|password, GET /admin/auth/me, GET|POST /admin/users,
// PATCH /admin/users/{id}, POST /admin/users/{id}/reset-password, GET /admin/stats, GET /admin/audit.
//
// Projects and keys live in PostgreSQL when persistence is on (DBStore) and in memory otherwise
// (MemStore, lost on restart). Sheets live in sheet.Registry either way; the persist writer
// records a created sheet through its OpCreate.
package api
