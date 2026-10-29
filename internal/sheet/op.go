package sheet

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Cross-package contract: internal/ws, internal/persist and internal/replica build on these types;
// a change here is a change to all three.

// OpKind names one applied mutation.
type OpKind uint8

const (
	OpCreate       OpKind = iota + 1 // a sheet was created (PasswordHash, ProjectID set)
	OpSet                            // Key, Value, ExpiresAt (set_null is an OpSet of protocol.NullValue)
	OpDelete                         // Key
	OpDeletePrefix                   // Prefix ("" = every key)
	OpDeleteSuffix                   // Suffix
	OpExpire                         // Keys removed by one janitor pass
)

// String is the op name for logs.
func (k OpKind) String() string {
	switch k {
	case OpCreate:
		return "create"
	case OpSet:
		return "set"
	case OpDelete:
		return "delete"
	case OpDeletePrefix:
		return "delete_prefix"
	case OpDeleteSuffix:
		return "delete_suffix"
	case OpExpire:
		return "expire"
	}
	return "unknown"
}

// Op is one mutation a sheet applied, in the order it applied them. Seq is the sheet sequence
// after the op (OpCreate carries the sheet's current Seq). Observers receive every Op.
type Op struct {
	Kind         OpKind
	Sheet        string
	ProjectID    uuid.UUID
	Seq          uint64
	Key          string
	Keys         []string
	Prefix       string
	Suffix       string
	Value        json.RawMessage
	ExpiresAt    time.Time // zero = no expiry
	PasswordHash []byte    // OpCreate only
}

// Observer receives every applied Op. Observe is called while the sheet's mutex is held, in Seq
// order per sheet, so it must not block, must not call back into the sheet, and must copy nothing
// it does not own (Value and Keys are not mutated after the call). The persist writer and the
// replica hub are observers; each enqueues and returns.
type Observer interface {
	Observe(op Op)
}

// Collaborator is one client connection joined to a sheet (internal/ws implements it).
type Collaborator interface {
	// ID is unique per connection for the life of the process (lock owner, logs).
	ID() uint64
	// Send enqueues one frame without blocking. It returns false when the connection's send
	// queue is full or the connection is closed; the sheet then calls Kick (D-014).
	Send(frame []byte) bool
	// Kick closes the connection without blocking; it resyncs on reconnect. Safe to call more
	// than once and from under the sheet's mutex.
	Kick(reason string)
}
