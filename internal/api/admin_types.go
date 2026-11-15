package api

// JSON bodies of the admin portal routes (S11, doc/specs/s11_admin_portal.md). They live here,
// not in protocol/: they are the admin port's REST surface, not the gRPC wire. Times are RFC 3339
// UTC ("" = none). No body ever carries a hash, a token or a stored password.

import (
	"encoding/json"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// BootstrapRequest is POST /admin/bootstrap.
type BootstrapRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// LoginRequest is POST /admin/auth/login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// PasswordChangeRequest is POST /admin/auth/password.
type PasswordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// CreateUserRequest is POST /admin/users.
type CreateUserRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"` // viewer | admin | super_admin
}

// PatchUserRequest is PATCH /admin/users/{id}; an absent field is left as it is.
type PatchUserRequest struct {
	Role     *string `json:"role,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
}

// UserInfo is one admin user as the portal shows it.
type UserInfo struct {
	UserID             string `json:"user_id"`
	Email              string `json:"email"`
	Name               string `json:"name"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"must_change_password"`
	LockedUntil        string `json:"locked_until,omitempty"`
	DisabledAt         string `json:"disabled_at,omitempty"`
	CreatedBy          string `json:"created_by,omitempty"`
	CreatedAt          string `json:"created_at"`
	PasswordChangedAt  string `json:"password_changed_at"`
	LastLoginAt        string `json:"last_login_at,omitempty"`
}

// MeResponse is GET /admin/auth/me and the body of a successful login. User is absent for the
// bearer admin key.
type MeResponse struct {
	Actor              string    `json:"actor"`
	Role               string    `json:"role"`
	MustChangePassword bool      `json:"must_change_password"`
	ExpiresAt          string    `json:"expires_at,omitempty"` // login only: the session's absolute end
	User               *UserInfo `json:"user,omitempty"`
}

// CreateUserResponse is POST /admin/users: the temporary password is shown this once.
type CreateUserResponse struct {
	User              UserInfo `json:"user"`
	TemporaryPassword string   `json:"temporary_password"`
}

// ResetPasswordResponse is POST /admin/users/{id}/reset-password.
type ResetPasswordResponse struct {
	TemporaryPassword string `json:"temporary_password"`
}

// NodeStats is one node's entry of GET /admin/stats: counts only (D-040), never a key or value.
type NodeStats struct {
	Node       string                `json:"node"`
	Role       string                `json:"role,omitempty"`
	Epoch      uint64                `json:"epoch,omitempty"`
	InstanceID string                `json:"instance_id,omitempty"`
	StartedAt  string                `json:"started_at,omitempty"`
	Stats      *protocol.HealthStats `json:"stats,omitempty"`
	Error      string                `json:"error,omitempty"` // the peer could not be read
}

// StatsResponse is GET /admin/stats: this node first, then the peer (unless ?scope=local).
type StatsResponse struct {
	Nodes []NodeStats `json:"nodes"`
}

// AuditEventInfo is one row of GET /admin/audit.
type AuditEventInfo struct {
	At        string          `json:"at"`
	Node      string          `json:"node"`
	ProjectID string          `json:"project_id,omitempty"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Subject   string          `json:"subject"`
	Detail    json.RawMessage `json:"detail"`
}
