// Package api is the HTTP management API (S6): the registry that lets a second project get its own
// sheet and API key without touching code (K14, R11, R12).
//
//	POST /admin/projects               admin key           → project + API key (SHA-256 at rest)
//	POST /admin/projects/{id}/sheets   admin or project key → a sheet readers can open
//	GET  /admin/sheets/{id}/export     admin or owner key   → every live entry (replaces `dump`)
//
// Projects and keys live in PostgreSQL when persistence is on (DBStore) and in memory otherwise
// (MemStore, lost on restart). Sheets live in sheet.Registry either way; the persist writer
// records a created sheet through its OpCreate.
package api
