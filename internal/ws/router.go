package ws

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/internal/lock"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// session is one connection's routing state. Only the connection's read goroutine touches it
// (handle, then end), so it needs no lock.
type session struct {
	h     *Handler
	c     *Conn
	sheet *sheet.Sheet // nil until open succeeds
}

// failure is an action's refusal: the wire string, the cause for the log, and whether it is an
// expected outcome (logged at Debug) rather than a failure (Warn).
type failure struct {
	msg      string
	err      error
	expected bool
	key      string // echoed on lock held
	// closeAfter sends the reply as the connection's last frame (SendAndClose, StatusGoingAway).
	closeAfter bool
	// then runs after the reply is queued (the stale-epoch step-down).
	then func()
}

// handle decodes one frame and routes it. Every reply echoes req.ID. A refused action is logged
// once, here.
//
// after advo_cache/ws/handler.go:120-216 (the read loop and handleAction): same open-first gate and
// switch, plus the leader and epoch gates (D-012), and one log line per refusal at the router.
func (s *session) handle(frame []byte) {
	var req protocol.Request
	if err := json.Unmarshal(frame, &req); err != nil {
		s.refuse(req, failure{msg: protocol.ErrMsgInvalidJSON, err: err})
		return
	}
	if f := s.route(req); f != nil {
		s.refuse(req, *f)
	}
}

func (s *session) route(req protocol.Request) *failure {
	if req.Action == protocol.ActionOpen {
		return s.open(req)
	}
	if req.Action == protocol.ActionRegisterReplica {
		return &failure{msg: protocol.ErrMsgUseReplicaPath}
	}
	if s.sheet == nil {
		return &failure{msg: protocol.ErrMsgMustOpen}
	}
	switch req.Action {
	case protocol.ActionGet:
		return s.get(req)
	case protocol.ActionDump:
		return &failure{msg: protocol.ErrMsgDumpMoved}
	case protocol.ActionSet, protocol.ActionSetNull, protocol.ActionDelete,
		protocol.ActionDeletePrefix, protocol.ActionDeleteSuffix, protocol.ActionLock, protocol.ActionUnlock:
		if f := s.gate(req); f != nil {
			return f
		}
	default:
		return &failure{msg: protocol.ErrMsgUnknownAction}
	}
	switch req.Action {
	case protocol.ActionSet:
		if req.Key == "" {
			return &failure{msg: protocol.ErrMsgKeyRequired}
		}
		return s.apply(req, sheet.Mutation{Kind: sheet.OpSet, Key: req.Key, Value: req.Value, TTLMinutes: req.TTL, BaseSeq: req.BaseSeq})
	case protocol.ActionSetNull:
		if req.Key == "" {
			return &failure{msg: protocol.ErrMsgKeyRequired}
		}
		return s.apply(req, sheet.Mutation{Kind: sheet.OpSet, Key: req.Key, Value: protocol.NullValue, TTLMinutes: req.TTL})
	case protocol.ActionDelete:
		if req.Key == "" {
			return &failure{msg: protocol.ErrMsgKeyRequired}
		}
		return s.apply(req, sheet.Mutation{Kind: sheet.OpDelete, Key: req.Key})
	case protocol.ActionDeletePrefix:
		return s.apply(req, sheet.Mutation{Kind: sheet.OpDeletePrefix, Prefix: req.Prefix})
	case protocol.ActionDeleteSuffix:
		return s.apply(req, sheet.Mutation{Kind: sheet.OpDeleteSuffix, Suffix: req.Suffix})
	case protocol.ActionLock:
		return s.lock(req)
	default: // ActionUnlock
		return s.unlock(req)
	}
}

// gate refuses a write on a replica (not leader) or from a client that has seen a newer leader.
func (s *session) gate(req protocol.Request) *failure {
	if !s.h.node.IsLeader() {
		return &failure{msg: protocol.ErrMsgNotLeader}
	}
	if e := s.h.node.Epoch(); req.Epoch > e {
		// The reply goes out first and closes this socket (the client must fail over); only then
		// is the higher epoch reported, because the node's step-down kicks every connection.
		return &failure{msg: protocol.ErrMsgStaleEpoch, closeAfter: true,
			then: func() { s.h.node.ObserveEpoch(req.Epoch) }}
	}
	return nil
}

// open joins a sheet. Wrong password: the reply, then the socket closes.
//
// after advo_cache/ws/handler.go:222-255 (handleOpen): GetOrCreateSheet + AddCollaborator + reply +
// a separate sheet_state write became one sheet.Join that enqueues the reply and the state under the
// sheet mutex; creation follows REDLITE_SHEET_AUTOCREATE (D-013).
func (s *session) open(req protocol.Request) *failure {
	if f := s.gate(req); f != nil {
		return f
	}
	if req.Sheet == "" || req.Password == "" {
		return &failure{msg: protocol.ErrMsgSheetAndPassword}
	}
	if s.sheet != nil {
		return &failure{msg: protocol.ErrMsgAlreadyOpen, err: errors.New("already in sheet " + s.sheet.Name())}
	}
	sh, _, err := s.h.reg.Open(req.Sheet, req.Password)
	switch {
	case errors.Is(err, sheet.ErrBadPassword):
		logger.L.Warn("WS: bad password", "sheet", req.Sheet, "remote", s.c.Remote(), "conn_id", s.c.ID())
		s.c.SendAndClose(s.encode(protocol.Response{ID: req.ID, Error: protocol.ErrMsgInvalidPassword}), websocket.StatusPolicyViolation, protocol.ErrMsgInvalidPassword)
		return nil
	case errors.Is(err, sheet.ErrSheetNotFound):
		return &failure{msg: protocol.ErrMsgSheetNotFound, err: err, expected: true}
	case errors.Is(err, sheet.ErrNameRequired):
		return &failure{msg: protocol.ErrMsgSheetAndPassword}
	case err != nil:
		return &failure{msg: protocol.ErrMsgInternal, err: err}
	}
	epoch := s.h.node.Epoch()
	s.sheet = sh
	sh.Join(s.c, epoch, func(seq uint64) []byte {
		return s.encode(protocol.Response{
			Success: true, ID: req.ID, Sheet: sh.Name(),
			ProtocolVersion: protocol.Version, Epoch: epoch, Seq: seq,
		})
	})
	return nil
}

// get answers from the sheet's store; a miss is an expected outcome.
//
// after advo_cache/ws/handler.go:271-282 (handleGet).
func (s *session) get(req protocol.Request) *failure {
	e, ok := s.sheet.Get(req.Key)
	if !ok {
		return &failure{msg: protocol.ErrMsgKeyNotFound, expected: true}
	}
	s.reply(protocol.Response{Success: true, ID: req.ID, Key: req.Key, Value: e.Value})
	return nil
}

// apply runs one mutation through sheet.Apply, which broadcasts it and enqueues the reply in seq
// order. A refusal (stale write, sheet full) is logged here; its reply was already sent.
//
// after advo_cache/ws/handler.go:284-356 (handleSet, handleDelete*): store, reply, broadcast and
// replicate as four separate steps became one Apply under the sheet mutex (D-014); set_null is an
// OpSet of protocol.NullValue (handler.go:438-451).
func (s *session) apply(req protocol.Request, m sheet.Mutation) *failure {
	res := s.sheet.Apply(s.c, m, func(r sheet.Result) []byte {
		resp := protocol.Response{Success: r.Err == nil, ID: req.ID, Seq: r.Seq}
		if r.Err != nil {
			resp.Error = errMsg(r.Err)
			return s.encode(resp)
		}
		switch m.Kind {
		case sheet.OpSet:
			resp.Key = m.Key
			resp.ExpiresInMS = r.ExpiresIn.Milliseconds()
		case sheet.OpDelete:
			resp.Key, resp.Deleted = m.Key, r.Deleted
		default:
			resp.Deleted = r.Deleted
		}
		return s.encode(resp)
	})
	if res.Err != nil {
		s.log(req, failure{msg: errMsg(res.Err), err: res.Err, expected: errors.Is(res.Err, sheet.ErrStaleWrite)})
	}
	return nil
}

func errMsg(err error) string {
	switch {
	case errors.Is(err, sheet.ErrStaleWrite):
		return protocol.ErrMsgStaleWrite
	case errors.Is(err, sheet.ErrSheetFull):
		return protocol.ErrMsgSheetFull
	default:
		return protocol.ErrMsgInternal
	}
}

// lock takes a lock owned by this connection; lock_ttl is SECONDS, 0 = REDLITE_LOCK_TTL.
//
// after advo_cache/ws/handler.go:371-405 (handleLock): the owner is the connection id, the second
// IsLocked lookup is gone, and nothing is replicated (locks die with their connection).
func (s *session) lock(req protocol.Request) *failure {
	if req.Key == "" {
		return &failure{msg: protocol.ErrMsgKeyRequired}
	}
	ttl := s.h.opts.LockTTL
	if req.LockTTL > 0 {
		ttl = time.Duration(req.LockTTL) * time.Second
	}
	if _, err := s.h.locks.TryAcquire(s.sheet.Name(), req.Key, s.c.ID(), ttl); err != nil {
		return &failure{msg: protocol.ErrMsgLockHeld, err: err, key: req.Key}
	}
	s.reply(protocol.Response{Success: true, ID: req.ID, Key: req.Key})
	return nil
}

// unlock releases a lock this connection owns.
//
// after advo_cache/ws/handler.go:407-436 (handleUnlock).
func (s *session) unlock(req protocol.Request) *failure {
	if req.Key == "" {
		return &failure{msg: protocol.ErrMsgKeyRequired}
	}
	err := s.h.locks.Release(s.sheet.Name(), req.Key, s.c.ID())
	switch {
	case errors.Is(err, lock.ErrNotOwner):
		return &failure{msg: protocol.ErrMsgNotLockOwner, err: err}
	case errors.Is(err, lock.ErrLockNotFound):
		return &failure{msg: protocol.ErrMsgLockNotFound, err: err}
	case err != nil:
		return &failure{msg: protocol.ErrMsgInternal, err: err}
	}
	s.reply(protocol.Response{Success: true, ID: req.ID, Key: req.Key})
	return nil
}

// end runs once the socket has closed: leave the sheet, release this connection's locks.
//
// after advo_cache/ws/handler.go:136-167 (onDisconnect): the delete-sheet-on-last-leave is gone
// (K13) and the lock release is O(locks owned).
func (s *session) end() {
	if s.sheet != nil {
		s.sheet.Leave(s.c)
	}
	if n := s.h.locks.ReleaseAll(s.c.ID()); n > 0 {
		s.c.log.Info("Lock: released on disconnect", "locks", n)
	}
}

// refuse logs f once and replies with it.
func (s *session) refuse(req protocol.Request, f failure) {
	s.log(req, f)
	resp := protocol.Response{ID: req.ID, Error: f.msg, Key: f.key}
	if f.closeAfter {
		s.c.SendAndClose(s.encode(resp), websocket.StatusGoingAway, f.msg)
	} else {
		s.reply(resp)
	}
	if f.then != nil {
		f.then()
	}
}

func (s *session) log(req protocol.Request, f failure) {
	attrs := []any{"action", req.Action, "sheet", s.sheetName(req), "error", f.msg}
	if f.err != nil {
		attrs = append(attrs, "err", f.err)
	}
	if f.expected {
		s.c.log.Debug("WS: action refused", attrs...)
		return
	}
	s.c.log.Warn("WS: action failed", attrs...)
}

func (s *session) sheetName(req protocol.Request) string {
	if s.sheet != nil {
		return s.sheet.Name()
	}
	return req.Sheet
}

// reply enqueues a direct reply; a full queue kicks the connection (D-014).
func (s *session) reply(resp protocol.Response) {
	if !s.c.Send(s.encode(resp)) {
		s.c.Kick("send queue full")
	}
}

// encode marshals a Response. Response holds only strings, numbers and json.RawMessage the server
// already parsed, so it cannot fail; a failure is still answered with the internal error.
func (s *session) encode(resp protocol.Response) []byte {
	b, err := json.Marshal(resp)
	if err != nil {
		s.c.log.Error("WS: reply encode failed", "err", err)
		b, _ = json.Marshal(protocol.Response{ID: resp.ID, Error: protocol.ErrMsgInternal})
	}
	return b
}
