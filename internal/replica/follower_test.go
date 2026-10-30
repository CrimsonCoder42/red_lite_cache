// Covers: internal/replica (follower control frames — D-027/D-028/D-030; cluster token header — D-031)
package replica

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/sheet"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

func testReplicaNode(t *testing.T, onControl func(protocol.ReplicaControl)) *Node {
	t.Helper()
	n, err := New(t.Context(), sheet.NewRegistry(sheet.Options{}), Options{
		Role: RoleReplica, HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second,
		OnControl: onControl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Each of the four control types reaches OnControl decoded; a frame below the replica's epoch is
// dropped as a stale leader frame and never reaches it; nil OnControl ignores the frame.
func TestFollowerControlFrames(t *testing.T) {
	var got []protocol.ReplicaControl
	n := testReplicaNode(t, func(c protocol.ReplicaControl) { got = append(got, c) })
	n.adopt(3)
	ap := &applier{n: n}
	sent := []protocol.ReplicaControl{
		{Type: protocol.TypeProjectSuspended, Project: "p1", Epoch: 3},
		{Type: protocol.TypeProjectResumed, Project: "p1", Epoch: 3},
		{Type: protocol.TypeKeyRevoked, Project: "p1", KeyID: "k1", Epoch: 3},
		{Type: protocol.TypeSessionKill, Session: "s1", Epoch: 4},
	}
	for _, c := range sent {
		b, _ := json.Marshal(c)
		if err := ap.apply(b); err != nil {
			t.Fatalf("apply %s: %v", c.Type, err)
		}
	}
	if len(got) != len(sent) {
		t.Fatalf("OnControl got %d frames, want %d: %+v", len(got), len(sent), got)
	}
	for i := range sent {
		if got[i] != sent[i] {
			t.Fatalf("frame %d: got %+v, want %+v", i, got[i], sent[i])
		}
	}
	if n.Epoch() != 4 {
		t.Fatalf("epoch = %d, want 4 (adopted from the control frame)", n.Epoch())
	}
	stale, _ := json.Marshal(protocol.ReplicaControl{Type: protocol.TypeSessionKill, Session: "s2", Epoch: 2})
	if err := ap.apply(stale); !errors.Is(err, errStaleLeader) {
		t.Fatalf("stale control frame: err = %v, want errStaleLeader", err)
	}
	if len(got) != len(sent) {
		t.Fatal("a stale control frame reached OnControl")
	}

	quiet := &applier{n: testReplicaNode(t, nil)}
	b, _ := json.Marshal(sent[0])
	if err := quiet.apply(b); err != nil {
		t.Fatalf("apply without OnControl: %v", err)
	}
}

// D-031: /replica refuses before the upgrade (401, no hub entry) without the token; the peer
// probe sends it.
func TestClusterTokenRefusalAndProbeHeader(t *testing.T) {
	n, err := New(t.Context(), sheet.NewRegistry(sheet.Options{}), Options{
		Role: RoleLeader, HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second,
		ClusterToken: "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer nope"} {
		req := httptest.NewRequest(http.MethodGet, protocol.PathReplica, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		n.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("auth %q: status %d, want 401", auth, rec.Code)
		}
	}

	var seen string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(protocol.Health{Status: "ok", Role: RoleLeader, Epoch: 1})
	}))
	defer peer.Close()
	if _, err := probe(t.Context(), peer.Client(), peer.URL+protocol.PathHealth, "tok"); err != nil {
		t.Fatal(err)
	}
	if seen != "Bearer tok" {
		t.Fatalf("probe Authorization = %q, want %q", seen, "Bearer tok")
	}
	if h := bearer(""); h != nil {
		t.Fatalf("bearer(\"\") = %v, want nil", h)
	}
}
