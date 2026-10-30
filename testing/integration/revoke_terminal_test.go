// Covers: POST /admin/projects/{id}/revoke terminal close (tenancy review finding 2), open-vs-revoke race (finding 1)
package integration

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// TestRevokeProjectIsTerminal: revoking a project closes its sessions with 4002 (key revoked), so
// a reader stops with LastError = ErrAdmitted instead of reconnecting forever into
// "not authorized" (tenancy.md: revoke = suspend + keys revoked + sheets dropped).
func TestRevokeProjectIsTerminal(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	r := NewReader(t, srv, name, func(c *reader.Config) { c.APIKey = p.APIKey })
	raw := tnMustOpen(t, srv, name, p.APIKey)

	if code := adminDo(t, srv, "POST", "/admin/projects/"+p.ProjectID+"/revoke", testAdminKey, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	tnClosedWith(t, raw, protocol.CloseKeyRevoked, time.Second)
	Eventually(t, "reader stops with ErrAdmitted", func() bool { return errors.Is(r.LastError(), reader.ErrAdmitted) })
	time.Sleep(300 * time.Millisecond) // several ReconnectMax: a reconnect would show as a new session
	if n := srv.App.Sessions().Len(); n != 0 || r.IsConnected() {
		t.Fatalf("after revoke: sessions=%d connected=%v, want 0/false", n, r.IsConnected())
	}
}

// TestRevokedKeyNeverAdmitsAgain: once a key is revoked on this node it is refused even by a
// store that still resolves it — the guard the open path uses when an open races the revoke.
func TestRevokedKeyNeverAdmitsAgain(t *testing.T) {
	srv := StartLeader(t, tnKeyMode)
	p, name := tnProject(t, srv)
	d := tnDetail(t, srv, p.ProjectID)
	if len(d.APIKeys) != 1 {
		t.Fatalf("keys = %+v", d.APIKeys)
	}
	if code := adminDo(t, srv, "DELETE", "/admin/keys/"+d.APIKeys[0].KeyID, testAdminKey, nil, nil); code/100 != 2 {
		t.Fatalf("revoke key: %d", code)
	}
	if _, resp := tnOpen(t, srv, name, p.APIKey); resp.Success || resp.Error != protocol.ErrMsgNotAuthorized {
		t.Fatalf("open with a revoked key = %+v", resp)
	}
}
