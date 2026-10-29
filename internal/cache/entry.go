package cache

import (
	"encoding/json"
	"time"
)

// Entry is one stored value and its expiry. Value is the JSON bytes the client sent, never decoded
// by the server. ExpiresAt is the zero time when the entry does not expire (TTL 0, D-003).
//
// Cross-package contract: internal/sheet, internal/persist and internal/replica build on this shape.
type Entry struct {
	Value     json.RawMessage
	ExpiresAt time.Time
}

// Expired reports whether e is past its expiry at now. An entry with no expiry never expires.
func (e Entry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

// Remaining is the lifetime e has left at now: 0 when it does not expire, at least 1ms otherwise
// (so a live entry never reports "no expiry" on the wire).
func (e Entry) Remaining(now time.Time) time.Duration {
	if e.ExpiresAt.IsZero() {
		return 0
	}
	return max(e.ExpiresAt.Sub(now), time.Millisecond)
}
