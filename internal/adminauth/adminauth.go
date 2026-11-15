// Package adminauth holds the admin portal's building blocks that do not need the API server: the
// three roles, the password and session-token rules, the client IP rule and the rate limiter
// (S11, doc/specs/s11_admin_portal.md). internal/api wires them; internal/persist reuses the
// session idle time for its purge. Nothing here logs or returns a secret.
package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Role is an admin user's role, ordered: a higher role may do everything a lower one may (D7).
type Role int

const (
	RoleNone Role = iota
	RoleViewer
	RoleAdmin
	RoleSuperAdmin
)

// ParseRole maps the admin_users.role CHECK values to a Role; anything else is RoleNone.
func ParseRole(s string) Role {
	switch s {
	case "viewer":
		return RoleViewer
	case "admin":
		return RoleAdmin
	case "super_admin":
		return RoleSuperAdmin
	}
	return RoleNone
}

// String is the stored form ("" for RoleNone).
func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleAdmin:
		return "admin"
	case RoleSuperAdmin:
		return "super_admin"
	}
	return ""
}

// Session, lockout and password parameters (spec value-source table: D1, D2, PLAN.md §8, NIST SP
// 800-63B-4 §3.1.1.2 for the 15-character single-factor minimum, bcrypt's 72-byte input limit).
const (
	BcryptCost   = 12
	SessionIdle  = 30 * time.Minute
	SessionTTL   = 12 * time.Hour
	LockAfter    = 5
	LockFor      = 15 * time.Minute
	MinPassword  = 15
	MaxPassword  = 72 // bytes: bcrypt rejects longer input (bcrypt.ErrPasswordTooLong)
	tokenBytes   = 32 // session token, as an API key (internal/api/api.go newKey)
	tempPwdBytes = 18 // 24 base64url characters, above MinPassword
)

// ErrPasswordPolicy is the one reply for a password outside [MinPassword, MaxPassword].
var ErrPasswordPolicy = errors.New("password must be 15 to 72 bytes")

// CheckPassword enforces the length rule (the only rule NIST SP 800-63B-4 asks of a verifier).
func CheckPassword(pw string) error {
	if len(pw) < MinPassword || len(pw) > MaxPassword {
		return ErrPasswordPolicy
	}
	return nil
}

// HashPassword is bcrypt at cost (BcryptCost in production; tests pass bcrypt.MinCost).
func HashPassword(pw string, cost int) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), cost)
	return string(h), err
}

// PasswordMatches compares pw with a bcrypt hash in constant time for equal-length inputs
// (bcrypt.CompareHashAndPassword uses subtle.ConstantTimeCompare on the derived hash).
func PasswordMatches(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// TempPassword is a server-generated password, returned once by user create and reset.
func TempPassword() (string, error) {
	return randomString(tempPwdBytes)
}

// NewToken returns a fresh session token for the cookie and its stored form (sha256).
func NewToken() (raw string, hash []byte, err error) {
	raw, err = randomString(tokenBytes)
	if err != nil {
		return "", nil, err
	}
	return raw, HashToken(raw), nil
}

// HashToken is the at-rest form of a session token, the same scheme as API keys.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
