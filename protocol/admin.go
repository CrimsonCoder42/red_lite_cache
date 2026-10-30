package protocol

import "encoding/json"

// Management API bodies (S6, internal/api). HTTP JSON, not WebSocket frames, but still wire types a
// consumer or an operator script reads, so they live here.
//
// Auth: `Authorization: Bearer <key>` — the REDLITE_ADMIN_KEY, or a project's API key where the
// route allows it.

// CreateProjectRequest is the POST /admin/projects body (admin key only).
type CreateProjectRequest struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"` // new (D-028): label of the project's first key
}

// CreateProjectResponse returns the new project and its API key. APIKey is shown once; the server
// keeps only its SHA-256.
type CreateProjectResponse struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	APIKey    string `json:"api_key"`
	KeyPrefix string `json:"key_prefix"`
}

// CreateSheetRequest is the POST /admin/projects/{id}/sheets body (admin key or that project's key).
type CreateSheetRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// CreateSheetResponse confirms the sheet a reader can now open.
type CreateSheetResponse struct {
	Sheet     string `json:"sheet"`
	ProjectID string `json:"project_id"`
}

// ExportEntry is one key of a sheet export.
type ExportEntry struct {
	Value       json.RawMessage `json:"value"`
	ExpiresInMS int64           `json:"expires_in_ms,omitempty"` // 0 = no expiry
}

// SheetExport is the GET /admin/sheets/{id}/export body (admin key or the owning project's key).
// It replaces the v1 `dump` action.
type SheetExport struct {
	Sheet     string                 `json:"sheet"`
	ProjectID string                 `json:"project_id"`
	Seq       uint64                 `json:"seq"`
	Count     int                    `json:"count"`
	Entries   map[string]ExportEntry `json:"entries"`
}

// APIError is every non-2xx management API body.
type APIError struct {
	Error string `json:"error"`
}

// ── Tenancy (D-026…D-030) ────────────────────────────────────────────────────────────────────
// Every listing is admin-key only and paged: `?limit=` (default 50, max 200) and `?cursor=` (opaque,
// the previous page's NextCursor; keyset on created_at,id). Times are RFC 3339 UTC.

// Page wraps one page of a listing. NextCursor is empty on the last page.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// IssueKeyRequest is the POST /admin/projects/{id}/keys body (and optional on rotate).
type IssueKeyRequest struct {
	Label string `json:"label,omitempty"` // free text for the operator ("api-instance-3"); never a secret
}

// IssuedKey returns a key once (issue, rotate). The server keeps only its SHA-256.
type IssuedKey struct {
	KeyID     string `json:"key_id"`
	ProjectID string `json:"project_id"`
	APIKey    string `json:"api_key"`
	KeyPrefix string `json:"key_prefix"`
	Label     string `json:"label,omitempty"`
}

// KeyInfo is one key in a listing: never the key or its hash.
type KeyInfo struct {
	KeyID      string `json:"key_id"`
	KeyPrefix  string `json:"key_prefix"`
	Label      string `json:"label,omitempty"`
	CreatedAt  string `json:"created_at"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// SheetInfo is one sheet of a project in GET /admin/projects/{id}.
type SheetInfo struct {
	Name          string `json:"name"`
	Keys          int    `json:"keys"`          // live entries
	Collaborators int    `json:"collaborators"` // sessions joined on this node
}

// ProjectSummary is one row of GET /admin/projects.
type ProjectSummary struct {
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	SuspendedAt string `json:"suspended_at,omitempty"`
	Keys        int    `json:"keys"`   // live (unrevoked) API keys
	Sheets      int    `json:"sheets"` // sheets owned
}

// ProjectDetail is GET /admin/projects/{id}.
type ProjectDetail struct {
	ProjectSummary
	APIKeys    []KeyInfo   `json:"api_keys"`
	SheetsList []SheetInfo `json:"sheets_list"`
}

// SessionInfo is one live session (D-026): one authenticated WebSocket connection on Node.
type SessionInfo struct {
	SessionID   string `json:"session_id"`
	ProjectID   string `json:"project_id"`
	Sheet       string `json:"sheet"`
	KeyID       string `json:"key_id,omitempty"` // empty for a password-mode admission
	Remote      string `json:"remote"`
	ConnectedAt string `json:"connected_at"`
	LastSeen    string `json:"last_seen"`
	Epoch       uint64 `json:"epoch"`
	FramesIn    uint64 `json:"frames_in"`
	FramesOut   uint64 `json:"frames_out"`
	Node        string `json:"node"` // instance id of the node holding the connection
}
