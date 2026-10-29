package protocol

import "encoding/json"

// Management API bodies (S6, internal/api). HTTP JSON, not WebSocket frames, but still wire types a
// consumer or an operator script reads, so they live here.
//
// Auth: `Authorization: Bearer <key>` — the REDLITE_ADMIN_KEY, or a project's API key where the
// route allows it.

// CreateProjectRequest is the POST /admin/projects body (admin key only).
type CreateProjectRequest struct {
	Name string `json:"name"`
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
