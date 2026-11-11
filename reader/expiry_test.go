package reader

import (
	"fmt"
	"testing"
	"time"
)

// TestReaderDropsExpiredOffline (S9 criterion 10, D-045): with the server gone, the reader's own
// expiry pass (its wheel, the same cache.Wheel the server uses) drops entries whose deadline passed,
// so the map shrinks without the server's invalidate{keys}; an expired entry is a miss before and
// after it is dropped (R4); entries with no expiry stay.
func TestReaderDropsExpiredOffline(t *testing.T) {
	f := newFakeServer(t)
	for i := range 100 {
		f.put(fmt.Sprintf("ttl:%03d", i), `1`, 1_000)
		f.put(fmt.Sprintf("keep:%03d", i), `1`, 0)
	}
	c := &manualClock{t: time.Now()}
	r := mustNewClock(t, testConfig(f.addr()), c)
	waitFor(t, "sheet_state loaded", func() bool { return r.Stats().Entries == 200 })
	if st := r.Stats(); st.WheelItems != 100 {
		t.Fatalf("wheel holds %d items for 100 expiring keys", st.WheelItems)
	}

	f.setDown(true)
	waitFor(t, "disconnected", func() bool { return !r.IsConnected() })
	c.advance(2 * time.Second)
	if _, ok := r.local.lookup("ttl:000", c.now()); ok { // on the reader's clock (localHas uses wall time)
		t.Fatal("expired entry served before the pass dropped it (R4)")
	}
	waitFor(t, "expired entries dropped offline", func() bool { return r.Stats().Entries == 100 })
	if st := r.Stats(); st.WheelItems != 0 {
		t.Fatalf("after the pass: %+v", st)
	}
	if !r.localHas("keep:000") {
		t.Fatal("an entry with no expiry was dropped")
	}
}
