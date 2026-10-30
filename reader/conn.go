package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// readLimit is the largest frame the reader accepts: a sheet_state carries the whole sheet.
const readLimit = 64 << 20

// errLinkLost is delivered to a request whose connection died before its reply arrived.
var errLinkLost = errors.New("reader: connection lost")

// errStaleEpoch: the server's open reply carried a lower epoch than one already seen (D-012).
var errStaleEpoch = errors.New("reader: stale epoch")

// serverClose is a terminal close (D-030): the server ended the session with code and the reader
// must not reconnect. It unwraps to ErrSuspended, ErrAdmitted or ErrKicked.
type serverClose struct {
	code     int
	sentinel error
}

func (e *serverClose) Error() string { return fmt.Sprintf("%v (close %d)", e.sentinel, e.code) }
func (e *serverClose) Unwrap() error { return e.sentinel }

// terminalClose maps a read error carrying close code 4001/4002/4003 to its serverClose; any other
// error (1000, 1001, 1006, network) is nil — those keep reconnect-forever (R1).
func terminalClose(err error) *serverClose {
	if err == nil {
		return nil
	}
	switch code := websocket.CloseStatus(err); code {
	case protocol.CloseSuspended:
		return &serverClose{code: int(code), sentinel: ErrSuspended}
	case protocol.CloseKeyRevoked:
		return &serverClose{code: int(code), sentinel: ErrAdmitted}
	case protocol.CloseKicked:
		return &serverClose{code: int(code), sentinel: ErrKicked}
	}
	return nil
}

type opKind uint8

const (
	opSet opKind = iota
	opDelete
	opDeletePrefix
	opDeleteSuffix
)

// op is one write: what goes on the wire and what the read loop applies locally when it is acked.
type op struct {
	kind    opKind
	arg     string          // key for set/delete, prefix, or suffix
	value   json.RawMessage // set
	ttl     int             // set, minutes
	baseSeq uint64          // set
}

func (o op) isDelete() bool { return o.kind != opSet }

func (o op) action() string {
	switch o.kind {
	case opSet:
		return protocol.ActionSet
	case opDelete:
		return protocol.ActionDelete
	case opDeletePrefix:
		return protocol.ActionDeletePrefix
	default:
		return protocol.ActionDeleteSuffix
	}
}

func (o op) request(id, epoch uint64) protocol.Request {
	req := protocol.Request{Action: o.action(), ID: id, Epoch: epoch}
	switch o.kind {
	case opSet:
		base := o.baseSeq
		req.Key, req.Value, req.TTL, req.BaseSeq = o.arg, o.value, o.ttl, &base
	case opDelete:
		req.Key = o.arg
	case opDeletePrefix:
		req.Prefix = o.arg
	case opDeleteSuffix:
		req.Suffix = o.arg
	}
	return req
}

// call is one request awaiting its reply.
type call struct {
	id   uint64
	sess *session
	op   op
	done chan result // buffered 1; written once
}

type result struct {
	resp protocol.Response
	err  error
}

// session is one live connection. pending and dead are guarded by Reader.cmu.
type session struct {
	addr   string
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	// wmu makes id assignment and the write one step, so request ids reach the wire in order. A v1
	// server echoes no id and answers in order; the oldest pending call is its reply.
	wmu     chanMutex
	pending map[uint64]*call
	dead    bool
	readErr error // set by the read loop before it exits
}

// chanMutex is a mutex a writer can give up on: a send blocked behind a slow write waits at most
// until its own deadline.
type chanMutex chan struct{}

func (m chanMutex) lock(ctx context.Context) error {
	select {
	case m <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m chanMutex) unlock() { <-m }

// run is the one connection goroutine: connect, serve until the link drops, back off, repeat,
// until the reader's context ends.
//
// after advo_cache/reader/reader.go:530-584: 3 tries per server then give up → forever, alternating
// LeaderAddr/BackupAddr, backoff ReconnectMin doubling to ReconnectMax with full jitter, reset after
// a connection that lasted (R1, D-004). A terminal close (4001/4002/4003, D-030) ends it for good:
// LastError is set, the reader closes itself, and New (if still waiting) returns the sentinel.
func (r *Reader) run() {
	// Every exit of run is the reader's end (Close, Config.Context, terminal close), and connect
	// returns only after its read loop exited: nothing applies a frame or requeues a delete after this.
	defer r.release()
	var addrs []string
	for _, a := range []string{r.cfg.LeaderAddr, r.cfg.BackupAddr} {
		if a != "" {
			addrs = append(addrs, a)
		}
	}
	stable := min(r.cfg.ReconnectMax, 10*time.Second)
	backoff := r.cfg.ReconnectMin
	idx, failures, sessions := 0, 0, 0
	for {
		start := time.Now()
		connected, err := r.connect(addrs[idx], sessions > 0)
		if r.ctx.Err() != nil {
			return
		}
		if sc, ok := errors.AsType[*serverClose](err); ok {
			r.closeByServer(addrs[idx], sc)
			return
		}
		if connected {
			sessions++
			failures = 0
			if time.Since(start) >= stable {
				backoff = r.cfg.ReconnectMin
			}
			// The address was healthy: try it again first.
		} else {
			failures++
			if errors.Is(err, ErrAuthFailed) {
				r.signalFirst(ErrAuthFailed)
			}
			// Throttled: attempts 1, 2, 4, 8, … are logged.
			if failures&(failures-1) == 0 {
				msg := "Reader: connect failed"
				if errors.Is(err, errStaleEpoch) {
					msg = "Reader: epoch refused"
				}
				r.log.Warn(msg, "addr", addrs[idx], "attempt", failures, "err", err)
			}
			idx = (idx + 1) % len(addrs)
		}
		wait := time.Duration(rand.Int64N(int64(backoff) + 1))
		backoff = min(backoff*2, r.cfg.ReconnectMax)
		t := time.NewTimer(wait)
		select {
		case <-r.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// closeByServer is the terminal path (D-030), run by run itself: record the sentinel, close the
// reader (writes return ErrClosed from now on) and release New. Close stays idempotent: the
// caller's Close later only waits for the goroutines.
func (r *Reader) closeByServer(addr string, sc *serverClose) {
	err := sc.sentinel
	r.lastErr.Store(&err)
	r.closed.Store(true)
	r.log.Warn("Reader: closed by server", "addr", addr, "code", sc.code, "err", err)
	r.signalFirst(err)
	r.cancel()
}

// connect runs one connection to addr from dial to teardown. It reports whether the reader reached
// connected on it. A terminal close of the session (during the handshake, the replay or after it)
// is returned as a *serverClose.
//
// after advo_cache/reader/reader.go:376-441: open, wait for success, wait for sheet_state, mark
// connected → also: version + epoch on open, epoch check on the reply, queued deletes replayed and
// acked before connected (R2, R3, D-005, D-012).
func (r *Reader) connect(addr string, reconnect bool) (bool, error) {
	sess, err := r.handshake(addr)
	if err != nil {
		return false, err
	}
	done := make(chan struct{})
	r.wg.Go(func() {
		defer close(done)
		r.readLoop(sess)
	})
	replayed, err := r.replay(sess)
	connected := err == nil
	since := time.Now()
	if connected {
		msg := "Reader: connected"
		if reconnect {
			msg = "Reader: reconnected"
		}
		r.log.Info(msg, "addr", addr, "epoch", r.epoch.Load(), "seq", r.local.appliedSeq(), "replayed", replayed)
		r.signalFirst(nil)
	} else {
		sess.cancel()
	}
	<-done
	_ = sess.conn.CloseNow()
	if sc := terminalClose(sess.readErr); sc != nil {
		return connected, sc
	}
	if r.ctx.Err() == nil {
		cause := sess.readErr
		if err != nil {
			cause = err
		}
		if connected {
			r.log.Warn("Reader: disconnected", "addr", addr, "connected_for", time.Since(since), "err", cause)
		}
	}
	return connected, err
}

// handshake dials addr, opens the sheet and loads its sheet_state. On success the session is
// registered as current, not yet connected.
func (r *Reader) handshake(addr string) (*session, error) {
	hctx, hcancel := context.WithTimeout(r.ctx, r.cfg.ConnectTimeout)
	defer hcancel()
	conn, _, err := websocket.Dial(hctx, addr, nil)
	if err != nil {
		return nil, fmt.Errorf("reader: dial %s: %w", addr, err)
	}
	conn.SetReadLimit(readLimit)
	fail := func(err error) (*session, error) {
		_ = conn.CloseNow()
		return nil, err
	}

	seen := r.epoch.Load()
	open, err := json.Marshal(protocol.Request{
		Action: protocol.ActionOpen, ID: r.nextID.Add(1), Version: protocol.Version, Epoch: seen,
		Sheet: r.cfg.Sheet, Password: r.cfg.Password, APIKey: r.cfg.APIKey,
	})
	if err != nil {
		return fail(fmt.Errorf("reader: encode open: %w", err))
	}
	if err := conn.Write(hctx, websocket.MessageText, open); err != nil {
		return fail(fmt.Errorf("reader: open %s: %w", addr, err))
	}

	// The open reply is the first frame without "type"; anything typed before it is ignored.
	var reply protocol.Response
	for {
		data, env, err := readFrame(hctx, conn)
		if sc := terminalClose(err); sc != nil {
			return fail(sc)
		}
		if err != nil {
			return fail(fmt.Errorf("reader: open %s: %w", addr, err))
		}
		if env.Type != "" {
			continue
		}
		if err := json.Unmarshal(data, &reply); err != nil {
			return fail(fmt.Errorf("reader: open %s: decode reply: %w", addr, err))
		}
		break
	}
	if !reply.Success {
		switch reply.Error {
		case protocol.ErrMsgInvalidPassword, protocol.ErrMsgNotAuthorized:
			// Not terminal: New fails with ErrAuthFailed; a later reconnect keeps retrying (an
			// operator may fix the key or the password while the consumer runs).
			return fail(fmt.Errorf("reader: open %s: %w", addr, ErrAuthFailed))
		case protocol.ErrMsgSuspended:
			// The server follows this reply with close 4001 (D-027): terminal either way.
			return fail(&serverClose{code: protocol.CloseSuspended, sentinel: ErrSuspended})
		}
		return fail(fmt.Errorf("reader: open %s: server: %s", addr, reply.Error))
	}
	if reply.Epoch < seen {
		return fail(fmt.Errorf("reader: open %s: epoch %d < highest seen %d: %w", addr, reply.Epoch, seen, errStaleEpoch))
	}
	for {
		cur := r.epoch.Load()
		if reply.Epoch <= cur || r.epoch.CompareAndSwap(cur, reply.Epoch) {
			break
		}
	}

	for {
		data, env, err := readFrame(hctx, conn)
		if sc := terminalClose(err); sc != nil {
			return fail(sc)
		}
		if err != nil {
			return fail(fmt.Errorf("reader: open %s: await sheet_state: %w", addr, err))
		}
		if env.Type != protocol.TypeSheetState {
			continue
		}
		var st protocol.SheetState
		if err := json.Unmarshal(data, &st); err != nil {
			return fail(fmt.Errorf("reader: open %s: decode sheet_state: %w", addr, err))
		}
		r.local.replace(st, r.now())
		break
	}

	sctx, scancel := context.WithCancel(r.ctx)
	sess := &session{
		addr: addr, conn: conn, ctx: sctx, cancel: scancel,
		wmu: make(chanMutex, 1), pending: make(map[uint64]*call),
	}
	r.cmu.Lock()
	r.sess = sess
	r.cmu.Unlock()
	return sess, nil
}

func readFrame(ctx context.Context, conn *websocket.Conn) ([]byte, protocol.Envelope, error) {
	var env protocol.Envelope
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, env, err
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, env, fmt.Errorf("decode frame: %w", err)
	}
	return data, env, nil
}

// replay sends every queued delete, in order, and waits for each ack; deletes queued meanwhile are
// taken next. When the queue is empty the reader becomes connected, atomically with that check
// (D-005: no read is served from a map the replay has not cleared).
func (r *Reader) replay(sess *session) (int, error) {
	total := 0
	for {
		if err := sess.wmu.lock(sess.ctx); err != nil {
			return total, errLinkLost
		}
		r.cmu.Lock()
		if sess.dead {
			r.cmu.Unlock()
			sess.wmu.unlock()
			return total, errLinkLost
		}
		if len(r.queue) == 0 {
			r.connected.Store(true)
			r.cmu.Unlock()
			sess.wmu.unlock()
			return total, nil
		}
		calls := make([]*call, 0, len(r.queue))
		for _, o := range r.queue {
			c := &call{id: r.nextID.Add(1), sess: sess, op: o, done: make(chan result, 1)}
			sess.pending[c.id] = c
			calls = append(calls, c)
		}
		r.queue = nil
		r.cmu.Unlock()
		// The ops are pending now: if the link dies, teardown puts them back at the queue's front.
		for _, c := range calls {
			if err := r.write(sess, c); err != nil {
				sess.wmu.unlock()
				return total, err
			}
		}
		sess.wmu.unlock()
		for _, c := range calls {
			resp, err := r.await(r.ctx, c)
			if err != nil {
				return total, fmt.Errorf("reader: replay %s %q: %w", c.op.action(), c.op.arg, err)
			}
			if !resp.Success {
				return total, fmt.Errorf("reader: replay %s %q: server: %s", c.op.action(), c.op.arg, resp.Error)
			}
			total++
		}
	}
}

// errQueued: the delete went to the replay queue instead of the wire.
var errQueued = errors.New("reader: queued")

// send registers o as pending on the current session and writes it. With queueIfDown, a delete on a session that is
// gone or not yet connected is queued instead (errQueued). A write error leaves the call pending;
// the session is cancelled and its teardown fails or requeues it.
func (r *Reader) send(o op, queueIfDown bool) (*call, error) {
	r.cmu.Lock()
	sess := r.sess
	r.cmu.Unlock()
	locked := false
	if sess != nil {
		wctx, cancel := context.WithTimeout(r.ctx, r.cfg.WriteTimeout)
		locked = sess.wmu.lock(wctx) == nil
		cancel()
	}
	unlock := func() {
		if locked {
			sess.wmu.unlock()
		}
	}
	r.cmu.Lock()
	if !locked || sess.dead || r.sess != sess || !r.connected.Load() {
		if queueIfDown {
			overflow := r.enqueueLocked(o)
			r.cmu.Unlock()
			unlock()
			r.logOverflow(overflow)
			return nil, errQueued
		}
		r.cmu.Unlock()
		unlock()
		if sess != nil && !locked && r.ctx.Err() == nil {
			return nil, ErrTimeout // a write ahead of this one held the connection for WriteTimeout
		}
		return nil, ErrNotConnected
	}
	c := &call{id: r.nextID.Add(1), sess: sess, op: o, done: make(chan result, 1)}
	sess.pending[c.id] = c
	r.cmu.Unlock()
	err := r.write(sess, c)
	unlock()
	return c, err
}

// write puts c on the wire; the caller holds sess.wmu. A failure cancels the session.
func (r *Reader) write(sess *session, c *call) error {
	data, err := json.Marshal(c.op.request(c.id, r.epoch.Load()))
	if err != nil {
		sess.cancel()
		return fmt.Errorf("reader: encode %s: %w", c.op.action(), err)
	}
	// Never the caller's ctx: coder/websocket closes the connection when a write's ctx ends.
	wctx, cancel := context.WithTimeout(sess.ctx, r.cfg.WriteTimeout)
	defer cancel()
	if err := sess.conn.Write(wctx, websocket.MessageText, data); err != nil {
		sess.cancel()
		return fmt.Errorf("reader: write %s: %w", c.op.action(), err)
	}
	return nil
}

// await waits for c's reply, at most WriteTimeout.
func (r *Reader) await(ctx context.Context, c *call) (protocol.Response, error) {
	t := time.NewTimer(r.cfg.WriteTimeout)
	defer t.Stop()
	select {
	case res := <-c.done:
		return res.resp, res.err
	case <-r.ctx.Done():
		return protocol.Response{}, ErrClosed
	case <-ctx.Done():
		return protocol.Response{}, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
	case <-t.C:
		return protocol.Response{}, ErrTimeout
	}
}

// enqueueLocked appends a delete to the replay queue; the caller holds cmu. Over MaxPendingDeletes
// the queue collapses to one delete_prefix "" (D-005); once collapsed nothing more is needed. A
// delete racing Close is dropped: a closed reader never replays, so it would only linger.
func (r *Reader) enqueueLocked(o op) (overflow bool) {
	if r.isClosed() {
		return false
	}
	if len(r.queue) == 1 && r.queue[0].kind == opDeletePrefix && r.queue[0].arg == "" {
		return false
	}
	r.queue = append(r.queue, o)
	return r.boundQueueLocked()
}

func (r *Reader) boundQueueLocked() bool {
	if len(r.queue) <= r.cfg.MaxPendingDeletes {
		return false
	}
	r.queue = []op{{kind: opDeletePrefix}}
	return true
}

func (r *Reader) logOverflow(overflow bool) {
	if overflow {
		r.log.Warn("Reader: delete queue overflow", "cap", r.cfg.MaxPendingDeletes, "replaced_by", "delete_prefix \"\"")
	}
}

// readLoop is the session's only reader: broadcasts and replies are applied here, in the order the
// server sent them.
func (r *Reader) readLoop(sess *session) {
	defer r.teardown(sess)
	defer sess.cancel()
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("Reader: panic recovered", "addr", sess.addr, "panic", p)
			sess.readErr = fmt.Errorf("reader: panic: %v", p)
		}
	}()
	for {
		data, env, err := readFrame(sess.ctx, sess.conn)
		if err != nil {
			sess.readErr = err
			return
		}
		if !r.dispatch(sess, env, data) {
			return
		}
	}
}

// dispatch applies one frame. It returns false when the session must end.
//
// after advo_cache/reader/reader.go:484-524: switch on type, replies ignored → replies matched to
// their request by id and applied in read order; expires_in_ms and seq honoured.
func (r *Reader) dispatch(sess *session, env protocol.Envelope, data []byte) bool {
	now := r.now()
	switch env.Type {
	case "":
		return r.reply(sess, env, data, now)
	case protocol.TypeSheetState:
		var m protocol.SheetState
		if json.Unmarshal(data, &m) == nil {
			r.local.replace(m, now)
		}
	case protocol.TypeReplicate:
		var m protocol.Replicate
		if json.Unmarshal(data, &m) == nil && m.Key != "" {
			r.local.put(m.Key, m.Value, expiry(now, m.ExpiresInMS), m.Seq, now)
		}
	case protocol.TypeInvalidate:
		var m protocol.Invalidate
		if json.Unmarshal(data, &m) == nil {
			r.local.invalidate(m)
		}
	}
	return true
}

func (r *Reader) reply(sess *session, env protocol.Envelope, data []byte, now time.Time) bool {
	r.cmu.Lock()
	c := sess.pending[env.ID]
	if env.ID == 0 { // v1 server: no id echoed, replies in request order
		for _, p := range sess.pending {
			if c == nil || p.id < c.id {
				c = p
			}
		}
	}
	if c == nil {
		r.cmu.Unlock()
		return true
	}
	if !env.Success && (env.Error == protocol.ErrMsgStaleEpoch || env.Error == protocol.ErrMsgNotLeader) {
		// This server is not the leader: leave the call pending so teardown requeues a delete, and
		// fail over.
		r.cmu.Unlock()
		r.log.Warn("Reader: server refused write", "addr", sess.addr, "action", c.op.action(), "error", env.Error)
		sess.readErr = fmt.Errorf("reader: server: %s", env.Error)
		return false
	}
	delete(sess.pending, c.id)
	r.cmu.Unlock()

	var resp protocol.Response
	if err := json.Unmarshal(data, &resp); err != nil {
		resp = protocol.Response{Success: false, Error: "reader: decode reply: " + err.Error()}
	}
	if resp.Success {
		if c.op.kind == opSet {
			r.local.put(c.op.arg, c.op.value, expiry(now, resp.ExpiresInMS), resp.Seq, now)
		} else {
			r.local.remove(c.op.kind, c.op.arg, resp.Seq)
		}
	}
	c.done <- result{resp: resp}
	return true
}

// teardown ends a session: not connected, unacked deletes back to the queue's front in their
// original order, every waiter failed with errLinkLost.
func (r *Reader) teardown(sess *session) {
	r.cmu.Lock()
	sess.dead = true
	if r.sess == sess {
		r.sess = nil
		r.connected.Store(false)
	}
	calls := make([]*call, 0, len(sess.pending))
	for _, c := range sess.pending {
		calls = append(calls, c)
	}
	sess.pending = nil
	slices.SortFunc(calls, func(a, b *call) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	var requeue []op
	for _, c := range calls {
		if c.op.isDelete() {
			requeue = append(requeue, c.op)
		}
	}
	overflow := false
	if len(requeue) > 0 {
		r.queue = append(requeue, r.queue...)
		overflow = r.boundQueueLocked()
	}
	r.cmu.Unlock()
	r.logOverflow(overflow)
	for _, c := range calls {
		select {
		case c.done <- result{err: errLinkLost}:
		default:
		}
	}
}
