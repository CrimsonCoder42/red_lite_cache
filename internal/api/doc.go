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
// Projects and keys live in PostgreSQL when persistence is on (DBStore) and in memory otherwise
// (MemStore, lost on restart). Sheets live in sheet.Registry either way; the persist writer
// records a created sheet through its OpCreate.
package api
