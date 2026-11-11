package cache

import (
	"testing"
	"time"
	"unsafe"
)

// timeHeader mirrors time.Time's fields (wall, ext, loc) so a test can move ONLY the monotonic
// reading, the way an NTP step or a suspended host makes the wall clock and the monotonic clock
// diverge. Go's time package has kept this layout since 1.9; shiftMono skips the test if it ever
// changes (no monotonic bit where expected).
type timeHeader struct {
	wall uint64
	ext  int64
	loc  *time.Location
}

func shiftMono(t *testing.T, at time.Time, d time.Duration) time.Time {
	t.Helper()
	h := (*timeHeader)(unsafe.Pointer(&at))
	if h.wall&(1<<63) == 0 {
		t.Skip("time.Time layout changed: no monotonic flag")
	}
	h.ext += int64(d)
	return at
}

// TestWheelWallClockJump (checker 2026-11-11, defect 1; D-045): keys are filed by their wall-clock
// second, so "due" must be decided on the wall clock too. With the monotonic reading still in the
// times (a leader's time.Now()), a wall clock that jumped an hour ahead of the monotonic one used to
// leave a key in a slot the wheel had already walked past: never removed, never broadcast, still
// counted against MaxKeys.
func TestWheelWallClockJump(t *testing.T) {
	t0 := time.Now() // carries a monotonic reading
	s := New()
	s.Set("k", Entry{Value: val("v"), ExpiresAt: t0.Add(10 * time.Second)})
	for i := range 100 { // a normal sheet: other keys in many later seconds
		s.Set(string(rune('A'+i%26))+string(rune('0'+i/26)), Entry{Value: val("v"), ExpiresAt: t0.Add(time.Duration(100_000+i) * time.Second)})
	}
	// The wall clock steps an hour ahead; the monotonic clock does not.
	at := func(wallSec int) time.Time {
		return shiftMono(t, t0.Add(time.Duration(wallSec)*time.Second), -time.Hour)
	}
	var removed []string
	for sec := 3605; sec <= 3605+600; sec++ { // a janitor pass every second for 10 minutes
		removed = append(removed, s.Expire(at(sec))...)
	}
	if len(removed) != 1 || removed[0] != "k" || s.Len() != 100 {
		t.Fatalf("removed %v, %d keys left: k must be expired once its wall-clock second has passed", removed, s.Len())
	}
}
