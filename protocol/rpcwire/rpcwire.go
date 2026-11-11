// Package rpcwire is the one translation between the gRPC contract (protocol/redlitev1, generated
// from proto/redlite/v1, D-048) and the JSON frame types of package protocol that the server's
// sheets, router and replica hub and the reader's session logic are written against. Server and
// reader both import it, so a frame means the same thing on both sides.
//
// Server side: ClientRequest (ClientMsg → protocol.Request), ServerMsgs (a JSON frame → one or more
// ServerMsg, a sheet state split into sorted chunks), ReplicaMsgs (the same for the replica
// stream), CloseStatus (a v1 close code → a gRPC status with ErrorDetail). Client side:
// ClientMsgFor, FrameFor (+ StateAssembler), ReplicaFrameFor (+ SnapshotAssembler), CloseCode.
package rpcwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	pb "github.com/CrimsonCoder42/red_lite_cache/protocol/redlitev1"
)

// DefaultChunkBytes bounds one StateChunk / SnapshotSheet: far under gRPC's default 4 MiB receive
// limit (grpc-go server.go defaultServerMaxReceiveMessageSize, dialoptions.go
// defaultClientMaxReceiveMessageSize), so no message of a large sheet is ever refused.
const DefaultChunkBytes = 1 << 20

// AuthHeader is the call-metadata key that carries `Bearer <API key>` (Cache) or `Bearer <cluster
// token>` (Replica).
const AuthHeader = "authorization"

// BearerFromMetadata returns the bearer token of md, "" when absent.
func BearerFromMetadata(md metadata.MD) string {
	for _, v := range md.Get(AuthHeader) {
		if t, ok := strings.CutPrefix(v, "Bearer "); ok {
			return t
		}
	}
	return ""
}

// entrySize is the approximate encoded size of one Entry (key, value, varints and tags).
func entrySize(k string, v []byte) int { return len(k) + len(v) + 24 }

// ── server side ─────────────────────────────────────────────────────────────────────────────

// ClientRequest turns a ClientMsg into the protocol.Request the router routes. apiKey is the
// stream's metadata key (D-024), put on the open request.
func ClientRequest(m *pb.ClientMsg, apiKey string) (protocol.Request, error) {
	req := protocol.Request{ID: m.GetId(), Epoch: m.GetEpoch()}
	switch b := m.GetBody().(type) {
	case *pb.ClientMsg_Open:
		req.Action, req.Sheet, req.Password, req.Version, req.APIKey = protocol.ActionOpen, b.Open.GetSheet(),
			b.Open.GetPassword(), int(b.Open.GetVersion()), apiKey
	case *pb.ClientMsg_Get:
		req.Action, req.Sheet, req.Key = protocol.ActionGet, b.Get.GetSheet(), b.Get.GetKey()
	case *pb.ClientMsg_Set:
		req.Action, req.Sheet, req.Key, req.TTL = protocol.ActionSet, b.Set.GetSheet(), b.Set.GetKey(), int(b.Set.GetTtlMinutes())
		req.Value = json.RawMessage(b.Set.GetValue())
		if b.Set.BaseSeq != nil {
			base := b.Set.GetBaseSeq()
			req.BaseSeq = &base
		}
	case *pb.ClientMsg_SetNull:
		req.Action, req.Sheet, req.Key, req.TTL = protocol.ActionSetNull, b.SetNull.GetSheet(), b.SetNull.GetKey(), int(b.SetNull.GetTtlMinutes())
	case *pb.ClientMsg_Delete:
		req.Action, req.Sheet, req.Key = protocol.ActionDelete, b.Delete.GetSheet(), b.Delete.GetKey()
	case *pb.ClientMsg_DeletePrefix:
		req.Action, req.Sheet, req.Prefix = protocol.ActionDeletePrefix, b.DeletePrefix.GetSheet(), b.DeletePrefix.GetPrefix()
	case *pb.ClientMsg_DeleteSuffix:
		req.Action, req.Sheet, req.Suffix = protocol.ActionDeleteSuffix, b.DeleteSuffix.GetSheet(), b.DeleteSuffix.GetSuffix()
	case *pb.ClientMsg_Lock:
		req.Action, req.Sheet, req.Key, req.LockTTL = protocol.ActionLock, b.Lock.GetSheet(), b.Lock.GetKey(), int(b.Lock.GetLockTtlSeconds())
	case *pb.ClientMsg_Unlock:
		req.Action, req.Sheet, req.Key = protocol.ActionUnlock, b.Unlock.GetSheet(), b.Unlock.GetKey()
	default:
		// An empty body, or a body this server does not know (a newer client): the router answers
		// "unknown action" (or "must open") echoing the id, and the stream stays up.
		req.Action = "unknown"
	}
	return req, nil
}

// ServerMsgs turns one server → client JSON frame (a Response, sheet_state, replicate or
// invalidate) into ServerMsgs. sheet names the session's sheet for frames that omit it; a
// sheet_state becomes sorted StateChunks of at most chunkBytes each (the last flagged).
func ServerMsgs(frame []byte, sheet string, chunkBytes int) ([]*pb.ServerMsg, error) {
	var env protocol.Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return nil, fmt.Errorf("rpcwire: decode frame: %w", err)
	}
	switch env.Type {
	case "":
		var r protocol.Response
		if err := json.Unmarshal(frame, &r); err != nil {
			return nil, fmt.Errorf("rpcwire: decode reply: %w", err)
		}
		return []*pb.ServerMsg{{Id: r.ID, Body: &pb.ServerMsg_Reply{Reply: &pb.Reply{
			Success: r.Success, Error: r.Error, Sheet: r.Sheet, Key: r.Key, Value: r.Value,
			Deleted: int64(r.Deleted), Count: int64(r.Count), ProtocolVersion: uint32(r.ProtocolVersion),
			Epoch: r.Epoch, Seq: r.Seq, ExpiresInMs: r.ExpiresInMS,
		}}}}, nil
	case protocol.TypeSheetState:
		var st protocol.SheetState
		if err := json.Unmarshal(frame, &st); err != nil {
			return nil, fmt.Errorf("rpcwire: decode sheet_state: %w", err)
		}
		var out []*pb.ServerMsg
		for i, part := range chunkEntries(st.Data, st.ExpiresInMS, chunkBytes) {
			out = append(out, &pb.ServerMsg{Body: &pb.ServerMsg_State{State: &pb.StateChunk{
				Sheet: sheet, Seq: st.Seq, Epoch: st.Epoch, Entries: part, Chunk: uint32(i),
			}}})
		}
		out[len(out)-1].GetState().Last = true
		return out, nil
	case protocol.TypeReplicate:
		var m protocol.Replicate
		if err := json.Unmarshal(frame, &m); err != nil {
			return nil, fmt.Errorf("rpcwire: decode replicate: %w", err)
		}
		return []*pb.ServerMsg{{Body: &pb.ServerMsg_Replicate{Replicate: replicateMsg(m, sheet)}}}, nil
	case protocol.TypeInvalidate:
		var m protocol.Invalidate
		if err := json.Unmarshal(frame, &m); err != nil {
			return nil, fmt.Errorf("rpcwire: decode invalidate: %w", err)
		}
		inv := &pb.Invalidate{Sheet: sheet, Seq: m.Seq}
		switch {
		case m.Prefix != nil:
			inv.Target = &pb.Invalidate_Prefix{Prefix: *m.Prefix}
		case m.Suffix != nil:
			inv.Target = &pb.Invalidate_Suffix{Suffix: *m.Suffix}
		case m.Keys != nil:
			inv.Target = &pb.Invalidate_Keys{Keys: &pb.KeyList{Keys: m.Keys}}
		default:
			inv.Target = &pb.Invalidate_Key{Key: m.Key}
		}
		return []*pb.ServerMsg{{Body: &pb.ServerMsg_Invalidate{Invalidate: inv}}}, nil
	}
	return nil, fmt.Errorf("rpcwire: no client message for frame type %q", env.Type)
}

func replicateMsg(m protocol.Replicate, sheet string) *pb.Replicate {
	if m.Sheet != "" {
		sheet = m.Sheet
	}
	return &pb.Replicate{Sheet: sheet, Key: m.Key, Value: m.Value, Seq: m.Seq, ExpiresInMs: m.ExpiresInMS, Epoch: m.Epoch}
}

// chunkEntries sorts data by key bytes and cuts it into runs of at most chunkBytes. An empty
// sheet is one empty chunk, so the final marker always exists.
func chunkEntries(data map[string]json.RawMessage, exp map[string]int64, chunkBytes int) [][]*pb.Entry {
	if chunkBytes <= 0 {
		chunkBytes = DefaultChunkBytes
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out [][]*pb.Entry
	var cur []*pb.Entry
	size := 0
	for _, k := range keys {
		v := data[k]
		if s := entrySize(k, v); size+s > chunkBytes && len(cur) > 0 {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, &pb.Entry{Key: k, Value: v, ExpiresInMs: exp[k]})
		size += entrySize(k, v)
	}
	return append(out, cur)
}

// ReplicaMsgs turns one leader → replica JSON frame (or the register reply) into ReplicaServerMsgs;
// a snapshot_sheet becomes sorted SnapshotSheet chunks of at most chunkBytes.
func ReplicaMsgs(frame []byte, chunkBytes int) ([]*pb.ReplicaServerMsg, error) {
	var env protocol.Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return nil, fmt.Errorf("rpcwire: decode replica frame: %w", err)
	}
	one := func(m *pb.ReplicaServerMsg) ([]*pb.ReplicaServerMsg, error) { return []*pb.ReplicaServerMsg{m}, nil }
	switch env.Type {
	case "":
		var r protocol.Response
		if err := json.Unmarshal(frame, &r); err != nil {
			return nil, err
		}
		return one(&pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_RegisterReply{RegisterReply: &pb.RegisterReply{
			Success: r.Success, Error: r.Error, Epoch: r.Epoch}}})
	case protocol.TypeSnapshotSheet:
		var s protocol.ReplicaSheet
		if err := json.Unmarshal(frame, &s); err != nil {
			return nil, err
		}
		var out []*pb.ReplicaServerMsg
		for _, part := range chunkEntries(s.Data, s.ExpiresInMS, chunkBytes) {
			out = append(out, &pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_SnapshotSheet{SnapshotSheet: &pb.SnapshotSheet{
				Sheet: s.Sheet, PasswordHash: s.PasswordHash, ProjectId: s.ProjectID, Entries: part, Seq: s.Seq, Epoch: s.Epoch,
			}}})
		}
		out[len(out)-1].GetSnapshotSheet().Last = true
		return out, nil
	case protocol.TypeReplicateSheet:
		var s protocol.ReplicaSheet
		if err := json.Unmarshal(frame, &s); err != nil {
			return nil, err
		}
		return one(&pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_ReplicateSheet{ReplicateSheet: &pb.ReplicaSheet{
			Sheet: s.Sheet, PasswordHash: s.PasswordHash, ProjectId: s.ProjectID, Seq: s.Seq, Epoch: s.Epoch}}})
	case protocol.TypeSnapshotEnd:
		var s protocol.SnapshotEnd
		if err := json.Unmarshal(frame, &s); err != nil {
			return nil, err
		}
		return one(&pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_SnapshotEnd{SnapshotEnd: &pb.SnapshotEnd{Sheets: int64(s.Sheets), Epoch: s.Epoch}}})
	case protocol.TypeHeartbeat:
		var h protocol.Heartbeat
		if err := json.Unmarshal(frame, &h); err != nil {
			return nil, err
		}
		return one(&pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_Heartbeat{Heartbeat: &pb.Heartbeat{Epoch: h.Epoch, InstanceId: h.InstanceID}}})
	case protocol.TypeReplicate:
		var m protocol.Replicate
		if err := json.Unmarshal(frame, &m); err != nil {
			return nil, err
		}
		return one(&pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_Replicate{Replicate: replicateMsg(m, "")}})
	case protocol.TypeReplicateDelete, protocol.TypeReplicateDeletePrefix, protocol.TypeReplicateDeleteSuffix, protocol.TypeReplicateExpire:
		var o protocol.ReplicaOp
		if err := json.Unmarshal(frame, &o); err != nil {
			return nil, err
		}
		op := &pb.ReplicaOp{Sheet: o.Sheet, Seq: o.Seq, Epoch: o.Epoch}
		switch env.Type {
		case protocol.TypeReplicateDelete:
			op.Kind, op.Target = pb.ReplicaOp_DELETE, &pb.ReplicaOp_Key{Key: o.Key}
		case protocol.TypeReplicateDeletePrefix:
			op.Kind, op.Target = pb.ReplicaOp_DELETE_PREFIX, &pb.ReplicaOp_Prefix{Prefix: deref(o.Prefix)}
		case protocol.TypeReplicateDeleteSuffix:
			op.Kind, op.Target = pb.ReplicaOp_DELETE_SUFFIX, &pb.ReplicaOp_Suffix{Suffix: deref(o.Suffix)}
		default:
			op.Kind, op.Target = pb.ReplicaOp_EXPIRE, &pb.ReplicaOp_Keys{Keys: &pb.KeyList{Keys: o.Keys}}
		}
		return one(&pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_Op{Op: op}})
	case protocol.TypeProjectSuspended, protocol.TypeProjectResumed, protocol.TypeKeyRevoked, protocol.TypeSessionKill:
		var c protocol.ReplicaControl
		if err := json.Unmarshal(frame, &c); err != nil {
			return nil, err
		}
		kind := map[string]pb.ReplicaControl_Kind{
			protocol.TypeProjectSuspended: pb.ReplicaControl_PROJECT_SUSPENDED, protocol.TypeProjectResumed: pb.ReplicaControl_PROJECT_RESUMED,
			protocol.TypeKeyRevoked: pb.ReplicaControl_KEY_REVOKED, protocol.TypeSessionKill: pb.ReplicaControl_SESSION_KILL,
		}[env.Type]
		return one(&pb.ReplicaServerMsg{Body: &pb.ReplicaServerMsg_Control{Control: &pb.ReplicaControl{
			Kind: kind, Project: c.Project, KeyId: c.KeyID, Session: c.Session, Epoch: c.Epoch}}})
	}
	return nil, fmt.Errorf("rpcwire: no replica message for frame type %q", env.Type)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Close codes the server uses besides the D-030 application codes (RFC 6455 numbers, kept as the
// one vocabulary for "why did my session end").
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	ClosePolicyViolation = 1008
	CloseMessageTooBig   = 1009
)

// CloseStatus is the status a stream ends with for a v1 close code and reason. The D-030 codes
// map to terminal statuses whose ErrorDetail.close_code a reader acts on; a policy violation (a
// refused open) is Unauthenticated; a slow consumer is ResourceExhausted; everything else is
// Unavailable (reconnect).
func CloseStatus(code int, reason string) error {
	var c codes.Code
	var why string
	switch {
	case code == CloseNormal:
		return nil
	case code == protocol.CloseSuspended:
		c, why = codes.PermissionDenied, "SUSPENDED"
	case code == protocol.CloseKeyRevoked:
		c, why = codes.Unauthenticated, "KEY_REVOKED"
	case code == protocol.CloseKicked:
		c, why = codes.Aborted, "KICKED"
	case code == ClosePolicyViolation && reason == protocol.ErrMsgInvalidPassword:
		c, why = codes.Unauthenticated, "INVALID_PASSWORD"
	case code == ClosePolicyViolation && reason == "stale epoch":
		c, why = codes.FailedPrecondition, "STALE_EPOCH"
	case code == ClosePolicyViolation:
		c, why = codes.Unauthenticated, "NOT_AUTHORIZED"
	case code == CloseMessageTooBig:
		c, why = codes.ResourceExhausted, "MESSAGE_TOO_LARGE"
	case reason == "send queue full":
		c, why = codes.ResourceExhausted, "SLOW_CONSUMER"
	case reason == protocol.ErrMsgNotLeader:
		c, why = codes.FailedPrecondition, "NOT_LEADER"
	default:
		c, why = codes.Unavailable, "GOING_AWAY"
	}
	st := status.New(c, reason)
	if d, err := st.WithDetails(&pb.ErrorDetail{Reason: why, Message: reason, CloseCode: int32(code)}); err == nil {
		st = d
	}
	return st.Err()
}

// ── client side ─────────────────────────────────────────────────────────────────────────────

// ClientMsgFor turns a protocol.Request into the ClientMsg that carries it (the API key goes in the
// call metadata, not here).
func ClientMsgFor(req protocol.Request) (*pb.ClientMsg, error) {
	m := &pb.ClientMsg{Id: req.ID, Epoch: req.Epoch}
	switch req.Action {
	case protocol.ActionOpen:
		m.Body = &pb.ClientMsg_Open{Open: &pb.Open{Sheet: req.Sheet, Password: req.Password, Version: uint32(req.Version)}}
	case protocol.ActionGet:
		m.Body = &pb.ClientMsg_Get{Get: &pb.Get{Sheet: req.Sheet, Key: req.Key}}
	case protocol.ActionSet:
		s := &pb.Set{Sheet: req.Sheet, Key: req.Key, Value: req.Value, TtlMinutes: uint32(max(req.TTL, 0))}
		if req.BaseSeq != nil {
			base := *req.BaseSeq
			s.BaseSeq = &base
		}
		m.Body = &pb.ClientMsg_Set{Set: s}
	case protocol.ActionSetNull:
		m.Body = &pb.ClientMsg_SetNull{SetNull: &pb.SetNull{Sheet: req.Sheet, Key: req.Key, TtlMinutes: uint32(max(req.TTL, 0))}}
	case protocol.ActionDelete:
		m.Body = &pb.ClientMsg_Delete{Delete: &pb.Delete{Sheet: req.Sheet, Key: req.Key}}
	case protocol.ActionDeletePrefix:
		m.Body = &pb.ClientMsg_DeletePrefix{DeletePrefix: &pb.DeletePrefix{Sheet: req.Sheet, Prefix: req.Prefix}}
	case protocol.ActionDeleteSuffix:
		m.Body = &pb.ClientMsg_DeleteSuffix{DeleteSuffix: &pb.DeleteSuffix{Sheet: req.Sheet, Suffix: req.Suffix}}
	case protocol.ActionLock:
		m.Body = &pb.ClientMsg_Lock{Lock: &pb.Lock{Sheet: req.Sheet, Key: req.Key, LockTtlSeconds: uint32(max(req.LockTTL, 0))}}
	case protocol.ActionUnlock:
		m.Body = &pb.ClientMsg_Unlock{Unlock: &pb.Unlock{Sheet: req.Sheet, Key: req.Key}}
	default:
		return nil, fmt.Errorf("rpcwire: action %q has no message on the Cache service", req.Action)
	}
	return m, nil
}

// UnknownMsg is a ClientMsg with no body: what a server sees from a client newer than itself. Only
// tests send it (the reader never sends an action the Cache service has no message for).
func UnknownMsg(id uint64) *pb.ClientMsg { return &pb.ClientMsg{Id: id} }

// FrameFor turns a ServerMsg into the JSON frame of package protocol it carries. A StateChunk is
// not a frame by itself: it returns (nil, nil) and goes to a StateAssembler instead.
func FrameFor(m *pb.ServerMsg) ([]byte, error) {
	switch b := m.GetBody().(type) {
	case *pb.ServerMsg_Reply:
		r := b.Reply
		return json.Marshal(protocol.Response{
			Success: r.GetSuccess(), ID: m.GetId(), Error: r.GetError(), Sheet: r.GetSheet(), Key: r.GetKey(),
			Value: rawOrNil(r.GetValue()), Deleted: int(r.GetDeleted()), Count: int(r.GetCount()),
			ProtocolVersion: int(r.GetProtocolVersion()), Epoch: r.GetEpoch(), Seq: r.GetSeq(), ExpiresInMS: r.GetExpiresInMs(),
		})
	case *pb.ServerMsg_Replicate:
		r := b.Replicate
		return json.Marshal(protocol.Replicate{Type: protocol.TypeReplicate, Key: r.GetKey(), Value: rawOrNil(r.GetValue()),
			Seq: r.GetSeq(), ExpiresInMS: r.GetExpiresInMs()})
	case *pb.ServerMsg_Invalidate:
		return json.Marshal(invalidateFrame(b.Invalidate))
	case *pb.ServerMsg_State:
		return nil, nil
	}
	return nil, errors.New("rpcwire: empty ServerMsg")
}

func invalidateFrame(i *pb.Invalidate) protocol.Invalidate {
	inv := protocol.Invalidate{Type: protocol.TypeInvalidate, Seq: i.GetSeq()}
	switch t := i.GetTarget().(type) {
	case *pb.Invalidate_Key:
		inv.Key = t.Key
	case *pb.Invalidate_Keys:
		inv.Keys = t.Keys.GetKeys()
	case *pb.Invalidate_Prefix:
		inv.Prefix = protocol.Ptr(t.Prefix)
	case *pb.Invalidate_Suffix:
		inv.Suffix = protocol.Ptr(t.Suffix)
	}
	return inv
}

// InvalidateFor is the protocol.Invalidate an Invalidate message carries.
func InvalidateFor(i *pb.Invalidate) protocol.Invalidate { return invalidateFrame(i) }

func rawOrNil(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// StateAssembler collects the StateChunks of one open into one protocol.SheetState.
type StateAssembler struct{ st *protocol.SheetState }

// Add takes the next chunk and returns the whole state once the last chunk has arrived.
func (a *StateAssembler) Add(c *pb.StateChunk) (*protocol.SheetState, bool) {
	if a.st == nil {
		a.st = &protocol.SheetState{Type: protocol.TypeSheetState, Data: map[string]json.RawMessage{}, Seq: c.GetSeq(), Epoch: c.GetEpoch()}
	}
	for _, e := range c.GetEntries() {
		a.st.Data[e.GetKey()] = json.RawMessage(e.GetValue())
		if ms := e.GetExpiresInMs(); ms > 0 {
			if a.st.ExpiresInMS == nil {
				a.st.ExpiresInMS = map[string]int64{}
			}
			a.st.ExpiresInMS[e.GetKey()] = ms
		}
	}
	if !c.GetLast() {
		return nil, false
	}
	st := a.st
	a.st = nil
	return st, true
}

// RegisterMsg is the replica's register as a ReplicaClientMsg.
func RegisterMsg(req protocol.Request) *pb.ReplicaClientMsg {
	return &pb.ReplicaClientMsg{Body: &pb.ReplicaClientMsg_Register{Register: &pb.Register{
		InstanceId: req.InstanceID, StartedAt: req.StartedAt, Epoch: req.Epoch}}}
}

// RegisterRequest is the protocol.Request a ReplicaClientMsg carries.
func RegisterRequest(m *pb.ReplicaClientMsg) (protocol.Request, error) {
	r := m.GetRegister()
	if r == nil {
		return protocol.Request{}, errors.New("rpcwire: empty ReplicaClientMsg")
	}
	return protocol.Request{Action: protocol.ActionRegisterReplica, InstanceID: r.GetInstanceId(), StartedAt: r.GetStartedAt(), Epoch: r.GetEpoch()}, nil
}

// SnapshotAssembler collects one sheet's SnapshotSheet chunks.
type SnapshotAssembler struct{ s *protocol.ReplicaSheet }

// ReplicaFrameFor turns a ReplicaServerMsg into the JSON frame the replica applier reads. A
// SnapshotSheet chunk that is not its sheet's last returns (nil, nil): asm keeps it until the
// last one, which returns the whole snapshot_sheet frame.
func ReplicaFrameFor(m *pb.ReplicaServerMsg, asm *SnapshotAssembler) ([]byte, error) {
	switch b := m.GetBody().(type) {
	case *pb.ReplicaServerMsg_RegisterReply:
		r := b.RegisterReply
		return json.Marshal(protocol.Response{Success: r.GetSuccess(), Error: r.GetError(), Epoch: r.GetEpoch()})
	case *pb.ReplicaServerMsg_SnapshotSheet:
		c := b.SnapshotSheet
		if asm.s == nil {
			asm.s = &protocol.ReplicaSheet{Type: protocol.TypeSnapshotSheet, Sheet: c.GetSheet(), PasswordHash: c.GetPasswordHash(),
				ProjectID: c.GetProjectId(), Seq: c.GetSeq(), Epoch: c.GetEpoch(), Data: map[string]json.RawMessage{}}
		}
		for _, e := range c.GetEntries() {
			asm.s.Data[e.GetKey()] = json.RawMessage(e.GetValue())
			if ms := e.GetExpiresInMs(); ms > 0 {
				if asm.s.ExpiresInMS == nil {
					asm.s.ExpiresInMS = map[string]int64{}
				}
				asm.s.ExpiresInMS[e.GetKey()] = ms
			}
		}
		if !c.GetLast() {
			return nil, nil
		}
		s := asm.s
		asm.s = nil
		return json.Marshal(s)
	case *pb.ReplicaServerMsg_SnapshotEnd:
		return json.Marshal(protocol.SnapshotEnd{Type: protocol.TypeSnapshotEnd, Sheets: int(b.SnapshotEnd.GetSheets()), Epoch: b.SnapshotEnd.GetEpoch()})
	case *pb.ReplicaServerMsg_Heartbeat:
		return json.Marshal(protocol.Heartbeat{Type: protocol.TypeHeartbeat, Epoch: b.Heartbeat.GetEpoch(), InstanceID: b.Heartbeat.GetInstanceId()})
	case *pb.ReplicaServerMsg_ReplicateSheet:
		s := b.ReplicateSheet
		return json.Marshal(protocol.ReplicaSheet{Type: protocol.TypeReplicateSheet, Sheet: s.GetSheet(), PasswordHash: s.GetPasswordHash(),
			ProjectID: s.GetProjectId(), Seq: s.GetSeq(), Epoch: s.GetEpoch()})
	case *pb.ReplicaServerMsg_Replicate:
		r := b.Replicate
		return json.Marshal(protocol.Replicate{Type: protocol.TypeReplicate, Sheet: r.GetSheet(), Key: r.GetKey(), Value: rawOrNil(r.GetValue()),
			Seq: r.GetSeq(), Epoch: r.GetEpoch(), ExpiresInMS: r.GetExpiresInMs()})
	case *pb.ReplicaServerMsg_Op:
		o := b.Op
		f := protocol.ReplicaOp{Sheet: o.GetSheet(), Seq: o.GetSeq(), Epoch: o.GetEpoch()}
		switch o.GetKind() {
		case pb.ReplicaOp_DELETE:
			f.Type, f.Key = protocol.TypeReplicateDelete, o.GetKey()
		case pb.ReplicaOp_DELETE_PREFIX:
			f.Type, f.Prefix = protocol.TypeReplicateDeletePrefix, protocol.Ptr(o.GetPrefix())
		case pb.ReplicaOp_DELETE_SUFFIX:
			f.Type, f.Suffix = protocol.TypeReplicateDeleteSuffix, protocol.Ptr(o.GetSuffix())
		default:
			f.Type, f.Keys = protocol.TypeReplicateExpire, o.GetKeys().GetKeys()
		}
		return json.Marshal(f)
	case *pb.ReplicaServerMsg_Control:
		c := b.Control
		typ := map[pb.ReplicaControl_Kind]string{
			pb.ReplicaControl_PROJECT_SUSPENDED: protocol.TypeProjectSuspended, pb.ReplicaControl_PROJECT_RESUMED: protocol.TypeProjectResumed,
			pb.ReplicaControl_KEY_REVOKED: protocol.TypeKeyRevoked, pb.ReplicaControl_SESSION_KILL: protocol.TypeSessionKill,
		}[c.GetKind()]
		return json.Marshal(protocol.ReplicaControl{Type: typ, Project: c.GetProject(), KeyID: c.GetKeyId(), Session: c.GetSession(), Epoch: c.GetEpoch()})
	}
	return nil, errors.New("rpcwire: empty ReplicaServerMsg")
}

// CloseCode reads the v1 close code a stream ended with: the ErrorDetail's close_code. A status
// gRPC itself raised carries no detail: a receive over the limit (RESOURCE_EXHAUSTED) is 1009. ok
// = false for anything else (a transport error, a plain end of stream). Never a D-030 code
// without the server's detail: a terminal close is always explicit.
func CloseCode(err error) (code int, reason string, ok bool) {
	st, isStatus := status.FromError(err)
	if !isStatus || st == nil {
		return 0, "", false
	}
	for _, d := range st.Details() {
		if e, ok := d.(*pb.ErrorDetail); ok {
			return int(e.GetCloseCode()), e.GetMessage(), true
		}
	}
	if st.Code() == codes.ResourceExhausted {
		return CloseMessageTooBig, st.Message(), true
	}
	return 0, "", false
}
