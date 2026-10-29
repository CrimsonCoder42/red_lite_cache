// Covers: lock, unlock, lock release on disconnect, lock TTL
package integration

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

func lockReq(key string, ttlSec int) protocol.Request {
	return protocol.Request{Action: protocol.ActionLock, Key: key, LockTTL: ttlSec}
}

func unlockReq(key string) protocol.Request {
	return protocol.Request{Action: protocol.ActionUnlock, Key: key}
}

// TestLockLoop is the LOCK LOOP: contention, release on disconnect, expiry after lock_ttl, and
// unlock of a lock someone else now holds.
func TestLockLoop(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	a, _ := OpenRaw(t, srv, name)
	b, _ := OpenRaw(t, srv, name)

	if r := a.Do(lockReq("L", 0)); !r.Success || r.Key != "L" {
		t.Fatalf("A lock = %+v", r)
	}
	if r := b.Do(lockReq("L", 0)); r.Success || r.Error != protocol.ErrMsgLockHeld || r.Key != "L" {
		t.Fatalf("B lock while A holds = %+v, want %q with key", r, protocol.ErrMsgLockHeld)
	}
	a.Conn.Close(websocket.StatusNormalClosure, "bye")
	Eventually(t, "B acquires L after A disconnects", func() bool { return b.Do(lockReq("L", 0)).Success })

	// lock_ttl is SECONDS: A2's 1s lock expires and B takes it; A2's unlock is then refused
	a2, _ := OpenRaw(t, srv, name)
	if r := a2.Do(lockReq("T", 1)); !r.Success {
		t.Fatalf("A2 lock T = %+v", r)
	}
	if r := b.Do(lockReq("T", 1)); r.Error != protocol.ErrMsgLockHeld {
		t.Fatalf("B lock T inside its TTL = %+v", r)
	}
	time.Sleep(1200 * time.Millisecond)
	if r := b.Do(lockReq("T", 5)); !r.Success {
		t.Fatalf("B lock T after expiry = %+v", r)
	}
	if r := a2.Do(unlockReq("T")); r.Error != protocol.ErrMsgNotLockOwner {
		t.Fatalf("A2 unlock T held by B = %+v, want %q", r, protocol.ErrMsgNotLockOwner)
	}
	if r := b.Do(unlockReq("T")); !r.Success || r.Key != "T" {
		t.Fatalf("B unlock T = %+v", r)
	}
	if r := b.Do(unlockReq("T")); r.Error != protocol.ErrMsgLockNotFound {
		t.Fatalf("second unlock = %+v, want %q", r, protocol.ErrMsgLockNotFound)
	}
}

// TestLockRelockRefresh: the owner re-locking refreshes the TTL, so the lock outlives its first TTL.
func TestLockRelockRefresh(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	a, _ := OpenRaw(t, srv, name)
	b, _ := OpenRaw(t, srv, name)
	if r := a.Do(lockReq("R", 1)); !r.Success {
		t.Fatalf("lock = %+v", r)
	}
	time.Sleep(600 * time.Millisecond)
	if r := a.Do(lockReq("R", 1)); !r.Success {
		t.Fatalf("re-lock by owner = %+v", r)
	}
	time.Sleep(600 * time.Millisecond) // 1.2s after the first lock, 0.6s into the refreshed one
	if r := b.Do(lockReq("R", 1)); r.Error != protocol.ErrMsgLockHeld {
		t.Fatalf("B lock after refresh = %+v, want held", r)
	}
	// locks are per sheet: the same key in another sheet is free
	other, _ := OpenRaw(t, srv, UniqueSheet(t))
	if r := other.Do(lockReq("R", 1)); !r.Success {
		t.Fatalf("same key, other sheet = %+v", r)
	}
}
