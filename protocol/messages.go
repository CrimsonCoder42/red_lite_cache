package protocol

import "encoding/json"

// Paths. Clients speak on PathWS; a replica speaks to its leader on PathReplica (D-010).
const (
	PathWS      = "/ws"
	PathReplica = "/replica"
	PathHealth  = "/health"
)

// Actions (client → server, Request.Action).
const (
	ActionOpen            = "open"
	ActionGet             = "get"
	ActionSet             = "set"
	ActionSetNull         = "set_null"
	ActionDelete          = "delete"
	ActionDeletePrefix    = "delete_prefix"
	ActionDeleteSuffix    = "delete_suffix"
	ActionDump            = "dump"
	ActionLock            = "lock"
	ActionUnlock          = "unlock"
	ActionRegisterReplica = "register_replica"
)

// Types (server → client and leader → replica, the "type" field).
const (
	TypeSheetState = "sheet_state"
	TypeReplicate  = "replicate"
	TypeInvalidate = "invalidate"

	TypeHeartbeat             = "heartbeat"
	TypeReplicateDelete       = "replicate_delete"
	TypeReplicateDeletePrefix = "replicate_delete_prefix"
	TypeReplicateDeleteSuffix = "replicate_delete_suffix"
	TypeReplicateExpire       = "replicate_expire" // new: janitor expiry, Keys batched per pass
	TypeReplicateSheet        = "replicate_sheet"  // new: a sheet was created (carries its password hash)
	TypeSnapshotSheet         = "snapshot_sheet"   // new: one sheet of the join snapshot (K6: chunked by sheet)
	TypeSnapshotEnd           = "snapshot_end"     // new: the join snapshot is complete
)

// NullPlaceholder is the value set_null stores, as a JSON string. A reader's IsNull is true for it
// and Get treats it as a miss the database already answered.
const NullPlaceholder = "__NULL__"

// NullValue is NullPlaceholder encoded as the JSON value the server stores and broadcasts.
var NullValue = json.RawMessage(`"` + NullPlaceholder + `"`)

// Error strings a reply carries in Response.Error. A reader matches on these, so they are part of
// the wire contract: never reword one that a v1 reader or backendAdvo may compare against.
const (
	ErrMsgInvalidJSON      = "invalid JSON"
	ErrMsgUnknownAction    = "unknown action"
	ErrMsgMustOpen         = "must open sheet first"
	ErrMsgAlreadyOpen      = "already in sheet"
	ErrMsgSheetAndPassword = "sheet and password required"
	ErrMsgInvalidPassword  = "invalid password"
	ErrMsgSheetNotFound    = "sheet not found"
	ErrMsgKeyRequired      = "key required"
	ErrMsgKeyNotFound      = "key not found"
	ErrMsgLockHeld         = "lock held by another client"
	ErrMsgNotLockOwner     = "not lock owner"
	ErrMsgLockNotFound     = "lock not found"
	ErrMsgStaleWrite       = "stale write" // new: base_seq is older than an invalidation covering the key (R6)
	ErrMsgStaleEpoch       = "stale epoch" // new: the request carries a higher epoch than this server (D-012)
	ErrMsgNotLeader        = "not leader"  // new: this instance is a replica; connect to the leader
	ErrMsgSheetFull        = "sheet full"  // new: REDLITE_CACHE_MAX_KEYS_PER_SHEET reached
	ErrMsgUseReplicaPath   = "register_replica moved to " + PathReplica
	ErrMsgDumpMoved        = "dump moved to GET /admin/sheets/{id}/export"
	ErrMsgInternal         = "internal error"
	// ErrMsgNotAuthorized is new (D-024): every admission refusal — missing key, a key of another
	// project, a revoked key, an unknown sheet in key mode — answers this one string, so a client
	// cannot tell which part was wrong (no oracle). The v1 "invalid password" stays in password mode.
	ErrMsgNotAuthorized = "not authorized"
	ErrMsgSuspended     = "suspended" // new (D-027): open of a suspended project; the socket closes 4001
)

// Request is every client → server frame, and the replica's register frame on PathReplica. It is a
// union: Action names which fields apply (doc/protocol/wire.md). One struct keeps one definition of
// each field and lets the server decode a frame in a single pass.
type Request struct {
	Action string `json:"action"`

	// ID is new (additive): a reader-chosen request id echoed in the Response, so a reply can be
	// matched to its request among interleaved broadcasts. Absent from a v1 reader.
	ID uint64 `json:"id,omitempty"`
	// Version is new (D-010): the protocol version the client speaks on open. Absent means 1.
	Version int `json:"version,omitempty"`
	// Epoch is new (D-012): the highest leader epoch the sender has seen. A server with a lower
	// epoch is stale and refuses the write with ErrMsgStaleEpoch.
	Epoch uint64 `json:"epoch,omitempty"`

	Sheet    string `json:"sheet,omitempty"`    // open
	Password string `json:"password,omitempty"` // open

	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"` // set: stored as bytes, never decoded by the server
	// TTL is MINUTES (v1). 0 or absent = the server default REDLITE_CACHE_TTL.
	TTL    int    `json:"ttl,omitempty"`
	Jitter bool   `json:"jitter,omitempty"` // v1: accepted and ignored; jitter is server policy
	Prefix string `json:"prefix,omitempty"` // delete_prefix; "" deletes every key in the sheet
	Suffix string `json:"suffix,omitempty"` // delete_suffix
	// LockTTL is SECONDS (v1). 0 or absent = REDLITE_LOCK_TTL.
	LockTTL int `json:"lock_ttl,omitempty"`

	// BaseSeq is new (R6): on set, the sheet sequence the sender had applied when it missed this
	// key. The server refuses the set (ErrMsgStaleWrite) when an invalidation covering the key was
	// applied after it. Absent = unconditional set (v1 behaviour). A pointer so 0 is expressible.
	BaseSeq *uint64 `json:"base_seq,omitempty"`

	// APIKey is new (D-024): on open, the project's API key. Required (and must belong to the
	// sheet's project) when the server runs REDLITE_ADMISSION=key; ignored in password mode.
	// Never logged (internal/logger masks api_key).
	APIKey string `json:"api_key,omitempty"`

	InstanceID string `json:"instance_id,omitempty"` // register_replica
	StartedAt  string `json:"started_at,omitempty"`  // register_replica, RFC3339
}

// Response is the direct reply to one Request. It never carries "type".
type Response struct {
	Success bool   `json:"success"`
	ID      uint64 `json:"id,omitempty"` // new: echoes Request.ID
	Error   string `json:"error,omitempty"`

	Sheet   string          `json:"sheet,omitempty"`
	Key     string          `json:"key,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Deleted int             `json:"deleted,omitempty"`
	Count   int             `json:"count,omitempty"`

	ProtocolVersion int    `json:"protocol_version,omitempty"` // new (D-010): on the open reply
	Epoch           uint64 `json:"epoch,omitempty"`            // new (D-012): on the open and register replies
	Seq             uint64 `json:"seq,omitempty"`              // new: the sheet sequence this write was applied at
	// ExpiresInMS is new (R4): on a set reply, the remaining lifetime the server gave the key, in
	// milliseconds after jitter. 0 = no expiry. Relative, so reader and server clocks need not agree.
	ExpiresInMS int64 `json:"expires_in_ms,omitempty"`
}

// SheetState is sent once to a client right after a successful open: the whole sheet.
type SheetState struct {
	Type string                     `json:"type"` // TypeSheetState
	Data map[string]json.RawMessage `json:"data"`
	// ExpiresInMS is new (R4): remaining lifetime per key in milliseconds; a key absent here does
	// not expire.
	ExpiresInMS map[string]int64 `json:"expires_in_ms,omitempty"`
	Seq         uint64           `json:"seq,omitempty"`   // new: the sheet sequence the state was taken at
	Epoch       uint64           `json:"epoch,omitempty"` // new
}

// Replicate is a set, broadcast to the sheet's other collaborators (no Sheet, no Epoch) and
// streamed to a replica (Sheet and Epoch set).
type Replicate struct {
	Type        string          `json:"type"` // TypeReplicate
	Sheet       string          `json:"sheet,omitempty"`
	Key         string          `json:"key"`
	Value       json.RawMessage `json:"value"`
	Seq         uint64          `json:"seq,omitempty"`           // new
	Epoch       uint64          `json:"epoch,omitempty"`         // new: replica stream only
	ExpiresInMS int64           `json:"expires_in_ms,omitempty"` // new (R4)
}

// Invalidate is a delete, broadcast to the sheet's other collaborators. Exactly one of Key, Keys,
// Prefix, Suffix is set. Prefix and Suffix are pointers so an empty prefix (every key) is
// distinguishable from absent; a v1 reader reads "prefix":"" as nothing to do.
type Invalidate struct {
	Type   string   `json:"type"` // TypeInvalidate
	Key    string   `json:"key,omitempty"`
	Keys   []string `json:"keys,omitempty"` // new (D-003): keys expired in one janitor pass
	Prefix *string  `json:"prefix,omitempty"`
	Suffix *string  `json:"suffix,omitempty"`
	Seq    uint64   `json:"seq,omitempty"` // new
}

// ReplicaOp is a delete-shaped op on the leader → replica stream: replicate_delete (Key),
// replicate_delete_prefix (Prefix), replicate_delete_suffix (Suffix), replicate_expire (Keys).
type ReplicaOp struct {
	Type   string   `json:"type"`
	Sheet  string   `json:"sheet"`
	Key    string   `json:"key,omitempty"`
	Keys   []string `json:"keys,omitempty"`
	Prefix *string  `json:"prefix,omitempty"`
	Suffix *string  `json:"suffix,omitempty"`
	Seq    uint64   `json:"seq,omitempty"`
	Epoch  uint64   `json:"epoch,omitempty"`
}

// ReplicaSheet carries a whole sheet to a replica: replicate_sheet when a sheet is created (no
// Data), snapshot_sheet for each sheet of the join snapshot. The password hash travels with the
// sheet so a promoted replica authenticates exactly as the leader did (K12).
type ReplicaSheet struct {
	Type         string                     `json:"type"`
	Sheet        string                     `json:"sheet"`
	PasswordHash []byte                     `json:"password_hash"` // bcrypt hash, base64 on the wire
	ProjectID    string                     `json:"project_id,omitempty"`
	Data         map[string]json.RawMessage `json:"data,omitempty"`
	ExpiresInMS  map[string]int64           `json:"expires_in_ms,omitempty"`
	Seq          uint64                     `json:"seq,omitempty"`
	Epoch        uint64                     `json:"epoch,omitempty"`
}

// SnapshotEnd closes the join snapshot; every frame after it is the live stream.
type SnapshotEnd struct {
	Type   string `json:"type"` // TypeSnapshotEnd
	Sheets int    `json:"sheets"`
	Epoch  uint64 `json:"epoch"`
}

// Heartbeat is sent by the leader to every replica each REDLITE_HEARTBEAT_INTERVAL.
type Heartbeat struct {
	Type       string `json:"type"` // TypeHeartbeat
	Epoch      uint64 `json:"epoch,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

// Envelope is the first decode of a server → client frame: Type tells a broadcast ("type" present)
// from a reply (absent), and ID matches a reply to its request.
type Envelope struct {
	Type    string `json:"type,omitempty"`
	ID      uint64 `json:"id,omitempty"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// Health is the GET /health body. A peer reads Role and Epoch to fence a stale leader (D-012).
type Health struct {
	Status     string `json:"status"`         // "ok"
	Role       string `json:"role,omitempty"` // "leader" | "replica"; omitted without the cluster token (D-031)
	Epoch      uint64 `json:"epoch,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	// Stats is D-040: this node's sizes, sent only when REDLITE_ENVIRONMENT=development (omitted
	// everywhere else). Diagnostic; not part of the compatibility contract.
	Stats *HealthStats `json:"stats,omitempty"`
}

// HealthStats is the development-only "stats" object of /health (D-040): counts only, never keys
// or values. The leak suite (testing/leaks) and a developer read it; fields may be added.
type HealthStats struct {
	Sheets          int    `json:"sheets"`
	Collaborators   int    `json:"collaborators"`
	Entries         int    `json:"entries"`
	RootBuckets     int    `json:"root_buckets"`
	TailBuckets     int    `json:"tail_buckets"`
	HeadBuckets     int    `json:"head_buckets"`
	SweepLogEntries int    `json:"sweep_log_entries"`
	Conns           int    `json:"conns"`
	Sessions        int    `json:"sessions"`
	LocksHeld       int    `json:"locks_held"`
	Suspended       int    `json:"suspended"`
	RevokedNames    int    `json:"revoked_names"`
	DeadKeys        int    `json:"dead_keys"`
	PersistQueued   int    `json:"persist_queued"`
	PersistParked   int    `json:"persist_parked"`
	PersistHeld     int    `json:"persist_held"`
	PersistDropped  uint64 `json:"persist_dropped"`
	Goroutines      int    `json:"goroutines"`
}

// Close codes (new, D-030): application codes in the 4000–4999 range RFC 6455 reserves for
// applications. A reader treats each as terminal — no reconnect — and reports it via LastError.
// Every other close (1000, 1001, 1006, network errors) keeps reconnect-forever (R1).
const (
	CloseSuspended  = 4001 // the project is suspended (D-027)
	CloseKeyRevoked = 4002 // the API key this session was admitted with was revoked or rotated (D-028)
	CloseKicked     = 4003 // an admin killed this session (D-030)
)

// Close reasons sent with the codes above.
const (
	CloseReasonSuspended  = "suspended"
	CloseReasonKeyRevoked = "key revoked"
	CloseReasonKicked     = "kicked by admin"
)

// Control frame types on the leader → replica stream (new, D-027/D-028/D-030): the tenancy changes
// a replica must apply when persistence is off (with persistence on, NOTIFY redlite_control carries
// them to every instance).
const (
	TypeProjectSuspended = "project_suspended"
	TypeProjectResumed   = "project_resumed"
	TypeKeyRevoked       = "key_revoked"
	TypeSessionKill      = "session_kill"
)

// ReplicaControl is one tenancy control frame on the replica stream. Project, KeyID and Session
// are identifiers (uuid strings) only — never a key, hash or password.
type ReplicaControl struct {
	Type    string `json:"type"`
	Project string `json:"project,omitempty"`
	KeyID   string `json:"key_id,omitempty"`
	Session string `json:"session,omitempty"`
	Epoch   uint64 `json:"epoch,omitempty"`
}

// Ptr returns a pointer to s, for Invalidate.Prefix/Suffix and ReplicaOp.Prefix/Suffix.
func Ptr(s string) *string { return &s }
