package sheet

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// Mutation is one client or admin write. Kind is OpSet, OpDelete, OpDeletePrefix or OpDeleteSuffix;
// set_null is an OpSet of protocol.NullValue.
type Mutation struct {
	Kind       OpKind
	Key        string
	Value      json.RawMessage
	TTLMinutes int // OpSet: 0 = Options.Policy.DefaultTTL
	Prefix     string
	Suffix     string
	// BaseSeq (R6, OpSet only): the seq the sender had applied when it missed Key. nil = unconditional.
	BaseSeq *uint64
}

// Result is what Apply did. On Err nothing was applied and Seq is the unchanged current seq.
type Result struct {
	Seq       uint64
	Deleted   int
	ExpiresIn time.Duration // OpSet: remaining lifetime after jitter; 0 = none
	Err       error         // ErrStaleWrite | ErrSheetFull (or an encode failure, never seen with valid JSON)
}

// Snapshot is a sheet's whole state, taken under its mutex.
type Snapshot struct {
	Name         string
	ProjectID    uuid.UUID
	PasswordHash []byte
	Seq          uint64
	Entries      map[string]cache.Entry
}

// Sheet is one tenant: a cache.Store, its collaborators and its sequence. One mutex orders every
// write with its broadcast, so every collaborator sees the sheet's ops in seq order (D-014, K10),
// and only the connection's own queue is ever written to (K9).
//
// after advo_cache/sheet/sheet.go:14-21: the embedded *cache.Cache became a private store, the
// RWMutex that guarded only the collaborator map became one Mutex around apply + broadcast, and
// collaborators are Collaborator queues keyed by ID instead of *websocket.Conn written directly.
type Sheet struct {
	name    string
	reg     *Registry
	project atomic.Pointer[uuid.UUID]
	hash    atomic.Pointer[[]byte]
	store   *cache.Store
	seq     atomic.Uint64 // written under mu; read lock-free by Seq

	mu      sync.Mutex
	collabs map[uint64]Collaborator
	log     sweepLog
	dropped bool // under mu: Registry.Drop removed this sheet; Apply refuses, Join kicks
}

func newSheet(r *Registry, project uuid.UUID, name string, hash []byte) *Sheet {
	s := &Sheet{
		name:    name,
		reg:     r,
		store:   cache.New(),
		collabs: make(map[uint64]Collaborator),
		log:     sweepLog{window: r.opts.SweepLogWindow, max: r.opts.SweepLogMax},
	}
	s.setIdentity(project, hash)
	return s
}

func (s *Sheet) setIdentity(project uuid.UUID, hash []byte) {
	s.project.Store(&project)
	h := append([]byte(nil), hash...)
	s.hash.Store(&h)
}

// Name is the sheet's name.
func (s *Sheet) Name() string { return s.name }

// ProjectID is the owning project.
func (s *Sheet) ProjectID() uuid.UUID { return *s.project.Load() }

// PasswordHash is the bcrypt hash (replication, persistence). Never log it.
func (s *Sheet) PasswordHash() []byte { return *s.hash.Load() }

// Seq is the sequence of the last applied op.
func (s *Sheet) Seq() uint64 { return s.seq.Load() }

// Len is the number of stored keys.
func (s *Sheet) Len() int { return s.store.Len() }

// CheckPassword reports whether pw matches the hash. A sheet with no hash refuses every password.
//
// after advo_cache/sheet/sheet.go:66-82 (ValidatePassword): the nil-hash branch that adopted the
// first caller's password (K12) is gone.
func (s *Sheet) CheckPassword(pw string) bool {
	h := s.PasswordHash()
	return len(h) > 0 && bcrypt.CompareHashAndPassword(h, []byte(pw)) == nil
}

// Get returns key's live entry. It takes only the store's read lock, never the sheet mutex.
func (s *Sheet) Get(key string) (cache.Entry, bool) { return s.store.Get(key, s.reg.now()) }

// Has reports whether key holds a live entry.
func (s *Sheet) Has(key string) bool { return s.store.Has(key, s.reg.now()) }

// Join adds c, then sends it openReply(seq) and the sheet_state frame, all under the sheet mutex, so
// every broadcast after Join reaches c after its sheet_state. openReply may be nil.
//
// after advo_cache/sheet/sheet.go:84-91 (AddCollaborator): the add now happens atomically with the
// state send (v1 sent full state outside any lock, ws/handler.go).
func (s *Sheet) Join(c Collaborator, epoch uint64, openReply func(seq uint64) []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped { // opened just before a revoke dropped the sheet: never join a removed sheet
		c.Kick("sheet revoked")
		return
	}
	seq := s.seq.Load()
	now := s.reg.now()
	all := s.store.All(now)
	st := protocol.SheetState{Type: protocol.TypeSheetState, Data: make(map[string]json.RawMessage, len(all)), Seq: seq, Epoch: epoch}
	for k, e := range all {
		st.Data[k] = e.Value
		if d := e.Remaining(now); d > 0 {
			if st.ExpiresInMS == nil {
				st.ExpiresInMS = make(map[string]int64)
			}
			st.ExpiresInMS[k] = d.Milliseconds()
		}
	}
	frame, err := json.Marshal(st)
	if err != nil {
		logger.L.Error("Sheet: state encode failed", "sheet", s.name, "conn_id", c.ID(), "err", err)
		c.Kick("sheet state encode failed")
		return
	}
	s.collabs[c.ID()] = c
	if openReply != nil && !s.sendLocked(c, openReply(seq)) {
		return
	}
	if !s.sendLocked(c, frame) {
		return
	}
	logger.L.Info("Sheet: joined", "sheet", s.name, "conn_id", c.ID(), "collaborators", len(s.collabs), "keys", len(all), "seq", seq)
}

// Leave removes c and returns how many collaborators remain. The sheet is never deleted (K13).
//
// after advo_cache/sheet/sheet.go:93-101 (RemoveCollaborator); the caller's delete-on-zero
// (ws/handler.go:160-164) is gone.
func (s *Sheet) Leave(c Collaborator) int {
	s.mu.Lock()
	_, ok := s.collabs[c.ID()]
	delete(s.collabs, c.ID())
	n := len(s.collabs)
	s.mu.Unlock()
	if ok {
		logger.L.Info("Sheet: left", "sheet", s.name, "conn_id", c.ID(), "collaborators", n)
	}
	return n
}

// sendLocked enqueues frame on c; a full or closed queue kicks c and drops it from the sheet
// (D-014). Caller holds mu.
func (s *Sheet) sendLocked(c Collaborator, frame []byte) bool {
	if c.Send(frame) {
		return true
	}
	delete(s.collabs, c.ID())
	c.Kick("send queue full")
	logger.L.Warn("Sheet: collaborator kicked", "sheet", s.name, "conn_id", c.ID(), "reason", "send queue full")
	return false
}

// fanoutLocked enqueues one encoded frame on every collaborator except sender and returns how many
// received it. Caller holds mu.
//
// after advo_cache/sheet/sheet.go:111-148 (Broadcast, BroadcastSetMsg): same sender exclusion, but
// the frame is encoded once by the caller and enqueued (never written) under the one sheet mutex.
func (s *Sheet) fanoutLocked(sender Collaborator, frame []byte) int {
	n := 0
	for id, c := range s.collabs {
		if sender != nil && id == sender.ID() {
			continue
		}
		if s.sendLocked(c, frame) {
			n++
		}
	}
	return n
}

// observe hands op to every observer. Caller holds mu.
func (s *Sheet) observe(op Op) {
	op.Sheet = s.name
	op.ProjectID = s.ProjectID()
	for _, o := range *s.reg.observers.Load() {
		o.Observe(op)
	}
}

// applyLocked encodes op's broadcast frame, applies op to the store, logs it for R6 (not expiry) and
// moves seq to op.Seq. On an encode error nothing is applied. Caller holds mu.
func (s *Sheet) applyLocked(op Op, now time.Time) (frame []byte, deleted int, err error) {
	switch op.Kind {
	case OpSet:
		var ms int64
		if !op.ExpiresAt.IsZero() {
			ms = cache.Entry{ExpiresAt: op.ExpiresAt}.Remaining(now).Milliseconds()
		}
		frame, err = json.Marshal(protocol.Replicate{Type: protocol.TypeReplicate, Key: op.Key, Value: op.Value, Seq: op.Seq, ExpiresInMS: ms})
	case OpDelete:
		frame, err = json.Marshal(protocol.Invalidate{Type: protocol.TypeInvalidate, Key: op.Key, Seq: op.Seq})
	case OpDeletePrefix:
		frame, err = json.Marshal(protocol.Invalidate{Type: protocol.TypeInvalidate, Prefix: protocol.Ptr(op.Prefix), Seq: op.Seq})
	case OpDeleteSuffix:
		frame, err = json.Marshal(protocol.Invalidate{Type: protocol.TypeInvalidate, Suffix: protocol.Ptr(op.Suffix), Seq: op.Seq})
	case OpExpire:
		frame, err = json.Marshal(protocol.Invalidate{Type: protocol.TypeInvalidate, Keys: op.Keys, Seq: op.Seq})
	default:
		return nil, 0, fmt.Errorf("sheet: apply %q: unsupported op %s", s.name, op.Kind)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("sheet: apply %q %s: encode: %w", s.name, op.Kind, err)
	}
	switch op.Kind {
	case OpSet:
		s.store.Set(op.Key, cache.Entry{Value: op.Value, ExpiresAt: op.ExpiresAt})
		s.log.add(op.Seq, op.Kind, op.Key, now)
	case OpDelete:
		if s.store.Delete(op.Key) {
			deleted = 1
		}
		s.log.add(op.Seq, op.Kind, op.Key, now)
	case OpDeletePrefix:
		deleted = s.store.DeletePrefix(op.Prefix)
		s.log.add(op.Seq, op.Kind, op.Prefix, now)
	case OpDeleteSuffix:
		deleted = s.store.DeleteSuffix(op.Suffix)
		s.log.add(op.Seq, op.Kind, op.Suffix, now)
	case OpExpire:
		for _, k := range op.Keys {
			if s.store.Delete(k) {
				deleted++
			}
		}
	}
	s.seq.Store(op.Seq)
	return frame, deleted, nil
}

// Apply performs m under the sheet mutex: R6 and MaxKeys checks, apply, seq++, ONE encoded
// broadcast to every collaborator except sender, then sender.Send(reply(res)), then every observer.
// On Err nothing is applied and the seq is unchanged; the reply is still sent. sender and reply may
// be nil.
func (s *Sheet) Apply(sender Collaborator, m Mutation, reply func(Result) []byte) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.reg.now()
	res := Result{Seq: s.seq.Load()}
	op := Op{Kind: m.Kind, Seq: res.Seq + 1, Key: m.Key, Prefix: m.Prefix, Suffix: m.Suffix}

	if s.dropped {
		// A session that opened this sheet before its project was revoked: nothing is applied or
		// observed, so no write reaches the persist writer after the revoke (second review, F).
		res.Err = fmt.Errorf("sheet: apply in %q: %w", s.name, ErrSheetNotFound)
	} else if m.Kind == OpSet {
		switch {
		case m.BaseSeq != nil && s.log.stale(*m.BaseSeq, m.Key, now):
			res.Err = fmt.Errorf("sheet: set %q in %q: %w", m.Key, s.name, ErrStaleWrite)
		case s.reg.opts.MaxKeys > 0 && s.store.Len() >= s.reg.opts.MaxKeys && !s.store.Has(m.Key, now):
			res.Err = fmt.Errorf("sheet: set %q in %q: %w", m.Key, s.name, ErrSheetFull)
		}
		op.Value = m.Value
		op.ExpiresAt = s.reg.opts.Policy.ExpiresAt(now, m.TTLMinutes)
	}

	var frame []byte
	fanout := 0
	if res.Err == nil {
		frame, res.Deleted, res.Err = s.applyLocked(op, now)
	}
	if res.Err == nil {
		res.Seq = op.Seq
		if !op.ExpiresAt.IsZero() {
			res.ExpiresIn = cache.Entry{ExpiresAt: op.ExpiresAt}.Remaining(now)
		}
		fanout = s.fanoutLocked(sender, frame)
	}
	if sender != nil && reply != nil {
		s.sendLocked(sender, reply(res))
	}
	if res.Err != nil {
		return res
	}
	s.observe(op)
	logger.L.Debug("Sheet: applied", "sheet", s.name, "op", op.Kind.String(), "seq", op.Seq, "fanout", fanout)
	return res
}

// ApplyReplicated applies one op from the leader's stream: an op at or below the current seq is
// ignored; otherwise it is applied with its own Seq and ExpiresAt, logged for R6, observed and
// broadcast to any collaborators (none on a replica). OpCreate updates the sheet's identity.
func (s *Sheet) ApplyReplicated(op Op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op.Kind == OpCreate {
		s.setIdentity(op.ProjectID, op.PasswordHash)
		s.observe(op)
		return
	}
	if op.Seq <= s.seq.Load() {
		return
	}
	frame, _, err := s.applyLocked(op, s.reg.now())
	if err != nil {
		logger.L.Error("Sheet: replicated op failed", "sheet", s.name, "op", op.Kind.String(), "seq", op.Seq, "err", err)
		return
	}
	s.fanoutLocked(nil, frame)
	s.observe(op)
}

// expire is one janitor pass: prune the sweep log by age (also when no key expires, so an idle
// sheet's log empties once the window has passed — the same prune stale() runs first, so R6 is
// unchanged), remove expired keys, seq++, ONE invalidate{keys} to every collaborator and one
// OpExpire to the observers (D-003, K3). Returns how many keys expired.
func (s *Sheet) expire(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.prune(now, false)
	keys := s.store.Expire(now)
	if len(keys) == 0 {
		return 0
	}
	op := Op{Kind: OpExpire, Seq: s.seq.Load() + 1, Keys: keys}
	frame, err := json.Marshal(protocol.Invalidate{Type: protocol.TypeInvalidate, Keys: keys, Seq: op.Seq})
	if err != nil { // []string always encodes; kept for the error contract
		logger.L.Error("Sheet: expire encode failed", "sheet", s.name, "keys", len(keys), "err", err)
		return len(keys)
	}
	s.seq.Store(op.Seq)
	fanout := s.fanoutLocked(nil, frame)
	s.observe(op)
	logger.L.Debug("Sheet: expired", "sheet", s.name, "keys", len(keys), "seq", op.Seq, "fanout", fanout)
	return len(keys)
}

// pruneLog prunes the sweep log by age and count at now (no key is touched) and returns how many
// entries it dropped.
func (s *Sheet) pruneLog(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.log.entries)
	s.log.prune(now, false)
	return before - len(s.log.entries)
}

// kickAll kicks and drops every collaborator; returns how many.
func (s *Sheet) kickAll(reason string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kickAllLocked(reason)
}

// drop marks the sheet removed (Registry.Drop) and kicks every collaborator, atomically with
// respect to Apply and Join.
func (s *Sheet) drop(reason string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped = true
	return s.kickAllLocked(reason)
}

func (s *Sheet) kickAllLocked(reason string) int {
	n := len(s.collabs)
	for id, c := range s.collabs {
		c.Kick(reason)
		delete(s.collabs, id)
	}
	return n
}

// stats returns the collaborator count, the sweep-log length and the store's counts, under mu.
func (s *Sheet) stats() (collabs, logEntries int, st cache.Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.collabs), len(s.log.entries), s.store.Stats()
}

// Snapshot runs fn with the sheet's whole state under the sheet mutex, so a caller that enqueues the
// snapshot does so atomically with respect to ops.
func (s *Sheet) Snapshot(fn func(Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(Snapshot{
		Name:         s.name,
		ProjectID:    s.ProjectID(),
		PasswordHash: s.PasswordHash(),
		Seq:          s.seq.Load(),
		Entries:      s.store.All(s.reg.now()),
	})
}

// Replace swaps entries, seq, project and hash for snap's (replica snapshot). The R6 log is reset
// with its floor at snap.Seq: no write older than the snapshot can be proven fresh.
func (s *Sheet) Replace(snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.Replace(snap.Entries)
	s.seq.Store(snap.Seq)
	s.setIdentity(snap.ProjectID, snap.PasswordHash)
	s.log.reset(snap.Seq)
}
