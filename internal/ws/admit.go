package ws

import (
	"errors"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
	"github.com/CrimsonCoder42/red_lite_cache/internal/session"
	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/internal/tenant"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// Admission (D-024, D-025, D-027) and the session lifecycle (D-026) of one connection.

// admit decides an open. It returns the sheet to join and the key id the connection was admitted
// with (uuid.Nil in password mode), or a nil sheet: then f is the refusal to reply with, or nil
// when the refusal reply is already queued and the socket closes behind it.
//
// Password mode is v1 exactly — "sheet not found", "invalid password" + close 1008, autocreate per
// REDLITE_SHEET_AUTOCREATE — except that a name a revoke dropped is never autocreated (D-025) and a
// suspended project answers "suspended" + close 4001. Key mode never autocreates: an unknown sheet,
// a missing, unknown, revoked or another project's key, and a wrong password all answer the one
// string "not authorized" + close 1008 (no oracle); the cause is only in the Warn log line.
func (s *client) admit(req protocol.Request) (sh *sheet.Sheet, keyID uuid.UUID, f *failure) {
	if s.h.tenants.KeyMode() {
		return s.admitKey(req)
	}
	sh, f = s.admitPassword(req)
	return sh, uuid.Nil, f
}

func (s *client) admitPassword(req protocol.Request) (*sheet.Sheet, *failure) {
	if _, ok := s.h.reg.Get(req.Sheet); !ok && s.h.tenants.Revoked(req.Sheet) {
		return nil, &failure{msg: protocol.ErrMsgSheetNotFound, err: errors.New("sheet revoked: never autocreated (D-025)")}
	}
	sh, _, err := s.h.reg.Open(req.Sheet, req.Password)
	switch {
	case errors.Is(err, sheet.ErrBadPassword):
		logger.L.Warn("WS: bad password", "sheet", req.Sheet, "remote", s.c.Remote(), "conn_id", s.c.ID())
		s.c.SendAndClose(s.encode(protocol.Response{ID: req.ID, Error: protocol.ErrMsgInvalidPassword}), websocket.StatusPolicyViolation, protocol.ErrMsgInvalidPassword)
		return nil, nil
	case errors.Is(err, sheet.ErrSheetNotFound):
		return nil, &failure{msg: protocol.ErrMsgSheetNotFound, err: err, expected: true}
	case errors.Is(err, sheet.ErrNameRequired):
		return nil, &failure{msg: protocol.ErrMsgSheetAndPassword}
	case err != nil:
		return nil, &failure{msg: protocol.ErrMsgInternal, err: err}
	}
	if _, err := s.h.tenants.Admit(s.ctx, sh.ProjectID(), ""); err != nil {
		return nil, s.refuseAdmission(req, err, "")
	}
	return sh, nil
}

func (s *client) admitKey(req protocol.Request) (*sheet.Sheet, uuid.UUID, *failure) {
	sh, ok := s.h.reg.Get(req.Sheet)
	if !ok {
		return nil, uuid.Nil, s.refuseAdmission(req, tenant.ErrNotAuthorized, "unknown sheet")
	}
	keyID, err := s.h.tenants.Admit(s.ctx, sh.ProjectID(), req.APIKey)
	if err != nil {
		reason := "key refused" // unknown, revoked, or another project's
		if req.APIKey == "" {
			reason = "missing key"
		}
		return nil, uuid.Nil, s.refuseAdmission(req, err, reason)
	}
	if !sh.CheckPassword(req.Password) {
		return nil, uuid.Nil, s.refuseAdmission(req, tenant.ErrNotAuthorized, "bad password")
	}
	return sh, keyID, nil
}

// refuseAdmission answers an Admit error. Suspended: "suspended" + close 4001. Not authorized:
// "not authorized" + close 1008. Both are logged once here, at Warn, with reason (never the key or
// the password) and return nil (the reply is queued). Any other error is a store failure: the
// router replies "internal error" and logs it.
func (s *client) refuseAdmission(req protocol.Request, err error, reason string) *failure {
	var (
		msg  string
		code websocket.StatusCode
	)
	switch {
	case errors.Is(err, tenant.ErrSuspended):
		msg, code, reason = protocol.ErrMsgSuspended, protocol.CloseSuspended, "suspended"
	case errors.Is(err, tenant.ErrNotAuthorized):
		msg, code = protocol.ErrMsgNotAuthorized, websocket.StatusPolicyViolation
	default:
		return &failure{msg: protocol.ErrMsgInternal, err: err}
	}
	logger.L.Warn("WS: admission refused", "sheet", req.Sheet, "remote", s.c.Remote(), "reason", reason, "conn_id", s.c.ID())
	closeReason := msg
	if code == protocol.CloseSuspended {
		closeReason = protocol.CloseReasonSuspended
	}
	s.c.SendAndClose(s.encode(protocol.Response{ID: req.ID, Error: msg}), code, closeReason)
	return nil
}

// startSession registers the admitted connection as a session (D-026) and binds it to the Conn, so
// every inbound frame touches it and every written frame counts. Its Kill is Conn.CloseWith: the
// peer reads the close code (4001/4002/4003, D-030). Audited as session_open.
func (s *client) startSession(sh *sheet.Sheet, keyID uuid.UUID, epoch uint64) *session.Session {
	c := s.c
	sess := session.New(session.Params{
		Project: sh.ProjectID(), Sheet: sh.Name(), KeyID: keyID, Remote: c.Remote(), Epoch: epoch,
		Kill: func(code int, reason string) { c.CloseWith(websocket.StatusCode(code), reason) },
	}, time.Now())
	c.bind(sess)
	s.h.sessions.Add(sess)
	logger.L.Info("WS: session opened", "session", sess.ID().String(), "project", sess.Project().String(),
		"sheet", sess.Sheet(), "key_id", keyIDString(keyID), "remote", c.Remote(), "conn_id", c.ID())
	s.h.audit.Audit(s.event(sess, "session_open", nil))
	return sess
}

// endSession removes the connection's session, if it had one, and audits session_close with the
// close status (-1 when the socket ended without a close frame).
func (s *client) endSession(err error) {
	sess := s.c.Session()
	if sess == nil {
		return
	}
	s.h.sessions.Remove(sess.ID())
	status := int(websocket.CloseStatus(err))
	logger.L.Info("WS: session closed", "session", sess.ID().String(), "project", sess.Project().String(),
		"sheet", sess.Sheet(), "status", status, "conn_id", s.c.ID())
	s.h.audit.Audit(s.event(sess, "session_close", map[string]string{"status": strconv.Itoa(status)}))
}

// event is a session audit row: identifiers only (session, sheet, remote, key_id) — never the key,
// its hash or the password (tenant.Event).
func (s *client) event(sess *session.Session, action string, extra map[string]string) tenant.Event {
	d := map[string]string{"session": sess.ID().String(), "sheet": sess.Sheet(), "remote": s.c.Remote()}
	if k := keyIDString(sess.KeyID()); k != "" {
		d["key_id"] = k
	}
	for k, v := range extra {
		d[k] = v
	}
	return tenant.Event{
		Project: sess.Project(), Actor: "session:" + sess.ID().String(), Action: action,
		Subject: sess.ID().String(), Detail: d,
	}
}

func keyIDString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
