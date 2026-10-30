// Package session is the per-node registry of live sessions (D-026). A session is one
// authenticated WebSocket connection: id, project, sheet, the API key it was admitted with,
// remote address, connected_at, last_seen, epoch and frame counters. Sessions live in memory on
// the node that holds the connection and die with it; open/close/kick go to audit_events.
//
// The hot path (every inbound frame) only touches atomics on the Session: no registry lock.
package session

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Kill closes a session's connection with an application close code (protocol.CloseSuspended,
// CloseKeyRevoked, CloseKicked) and a reason. It must not block (internal/ws: SendAndClose / close).
type Kill func(code int, reason string)

// Params are the fixed facts of a session, known at admission.
type Params struct {
	Project uuid.UUID
	Sheet   string
	KeyID   uuid.UUID // uuid.Nil for a password-mode admission
	Remote  string
	Epoch   uint64
	Kill    Kill
}

// Session is one live session. Every field but the atomics is fixed at New.
type Session struct {
	id          uuid.UUID
	p           Params
	connectedAt time.Time
	lastSeen    atomic.Int64 // unix nanos; Touch on every inbound frame
	in, out     atomic.Uint64
	killed      atomic.Bool
}

// New makes a session with a fresh id, connected and last seen at now.
func New(p Params, now time.Time) *Session {
	s := &Session{id: uuid.New(), p: p, connectedAt: now}
	s.lastSeen.Store(now.UnixNano())
	return s
}

// ID is the session id (also the conn's admin handle: DELETE /admin/sessions/{id}).
func (s *Session) ID() uuid.UUID { return s.id }

// Project, Sheet and KeyID are what the session was admitted for.
func (s *Session) Project() uuid.UUID { return s.p.Project }
func (s *Session) Sheet() string      { return s.p.Sheet }
func (s *Session) KeyID() uuid.UUID   { return s.p.KeyID }

// Touch records an inbound frame at now: last_seen and frames_in. Lock-free.
func (s *Session) Touch(now time.Time) {
	s.lastSeen.Store(now.UnixNano())
	s.in.Add(1)
}

// Sent counts one outbound frame.
func (s *Session) Sent() { s.out.Add(1) }

// Kill closes the session's connection once; later calls do nothing. Reports whether this call
// closed it.
func (s *Session) Kill(code int, reason string) bool {
	if !s.killed.CompareAndSwap(false, true) {
		return false
	}
	if s.p.Kill != nil {
		s.p.Kill(code, reason)
	}
	return true
}

// Info is a point-in-time copy of a session for listings.
type Info struct {
	ID          uuid.UUID
	Project     uuid.UUID
	Sheet       string
	KeyID       uuid.UUID
	Remote      string
	ConnectedAt time.Time
	LastSeen    time.Time
	Epoch       uint64
	FramesIn    uint64
	FramesOut   uint64
	Node        string
}

func (s *Session) info(node string) Info {
	return Info{
		ID: s.id, Project: s.p.Project, Sheet: s.p.Sheet, KeyID: s.p.KeyID, Remote: s.p.Remote,
		ConnectedAt: s.connectedAt, LastSeen: time.Unix(0, s.lastSeen.Load()), Epoch: s.p.Epoch,
		FramesIn: s.in.Load(), FramesOut: s.out.Load(), Node: node,
	}
}

// Registry is every live session on this node, keyed by session id (map + RWMutex, D-026).
type Registry struct {
	node string
	mu   sync.RWMutex
	m    map[uuid.UUID]*Session
}

// NewRegistry makes the registry for the node with instance id node (tagged on every Info).
func NewRegistry(node string) *Registry {
	return &Registry{node: node, m: map[uuid.UUID]*Session{}}
}

// Node is the instance id this registry tags its sessions with.
func (r *Registry) Node() string { return r.node }

// Add registers s.
func (r *Registry) Add(s *Session) {
	r.mu.Lock()
	r.m[s.id] = s
	r.mu.Unlock()
}

// Remove drops id (the connection ended). Reports whether it was registered.
func (r *Registry) Remove(id uuid.UUID) bool {
	r.mu.Lock()
	_, ok := r.m[id]
	delete(r.m, id)
	r.mu.Unlock()
	return ok
}

// Get returns the live session id.
func (r *Registry) Get(id uuid.UUID) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.m[id]
	return s, ok
}

// Len is the number of live sessions.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.m)
}

// Filter selects sessions in List; zero fields match everything.
type Filter struct {
	Project uuid.UUID
	Sheet   string
	KeyID   uuid.UUID
}

func (f Filter) match(s *Session) bool {
	return (f.Project == uuid.Nil || s.p.Project == f.Project) &&
		(f.Sheet == "" || s.p.Sheet == f.Sheet) &&
		(f.KeyID == uuid.Nil || s.p.KeyID == f.KeyID)
}

// List returns the matching sessions ordered by (connected_at, id) — the keyset the admin API
// pages on.
func (r *Registry) List(f Filter) []Info {
	r.mu.RLock()
	out := make([]Info, 0, len(r.m))
	for _, s := range r.m {
		if f.match(s) {
			out = append(out, s.info(r.node))
		}
	}
	r.mu.RUnlock()
	slices.SortFunc(out, func(a, b Info) int {
		if c := a.ConnectedAt.Compare(b.ConnectedAt); c != 0 {
			return c
		}
		return slices.Compare(a.ID[:], b.ID[:])
	})
	return out
}

// KillWhere kills every matching session with code and reason and returns how many it closed.
// Kill runs outside the registry lock (it is non-blocking, but must not be able to re-enter).
func (r *Registry) KillWhere(f Filter, code int, reason string) int {
	r.mu.RLock()
	var victims []*Session
	for _, s := range r.m {
		if f.match(s) {
			victims = append(victims, s)
		}
	}
	r.mu.RUnlock()
	n := 0
	for _, s := range victims {
		if s.Kill(code, reason) {
			n++
		}
	}
	return n
}
