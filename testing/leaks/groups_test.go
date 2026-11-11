// Covers: memory and goroutine bounds

package leaks

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
)

// L15 (S9, D-044/D-045): the server store's ordered index and expiry wheel drain to zero through
// group deletes alone, on two differently shaped key sets. 20 000 keys with expiries (so the wheel
// fills), every count rises, then suffix groups take half and prefix groups the rest, and Stats is
// all zero: no node, no wheel item, no wheel slot left.
func TestL15GroupsDrain(t *testing.T) {
	s := NewSteps(t)
	st := cache.New()
	base := st.Stats()
	s.Baseline("store %+v", base)
	storeZero(t, "baseline", base)

	exp := time.Now().Add(time.Hour)
	v := json.RawMessage(`{"v":1}`)
	var colon, path []string
	for i := range 10_000 {
		colon = append(colon, fmt.Sprintf("t%d:owner:%05d:%s", i%8, i/8, []string{"list", "count"}[i%2]))
		path = append(path, fmt.Sprintf("org-%02d/site-%03d/dev-%05d/m-%s", i%37, i%211, i, []string{"temp", "volt"}[i%2]))
	}
	for i, k := range append(append([]string{}, colon...), path...) {
		st.Set(k, cache.Entry{Value: v, ExpiresAt: exp.Add(time.Duration(i%3600) * time.Second)})
	}
	s.Fill("20 000 keys, two shapes, expiries over an hour")
	peak := st.Stats()
	s.Observe("store %+v", peak)
	Above(t, "forward nodes", peak.ForwardNodes, base.ForwardNodes)
	Above(t, "reverse nodes", peak.ReverseNodes, base.ReverseNodes)
	Above(t, "wheel items", peak.WheelItems, base.WheelItems)
	Above(t, "wheel slots", peak.WheelSlots, base.WheelSlots)

	n := st.DeleteSuffix(":list") + st.DeleteSuffix("/m-temp") // half of each shape
	for r := range 8 {
		n += st.DeletePrefix(fmt.Sprintf("t%d:", r))
	}
	for o := range 37 {
		n += st.DeletePrefix(fmt.Sprintf("org-%02d/", o))
	}
	s.Drain("suffix groups then prefix groups removed %d", n)
	if n != 20_000 {
		t.Errorf("group deletes removed %d, want 20000", n)
	}
	end := st.Stats()
	s.Assert("store %+v", end)
	storeZero(t, "after group deletes", end)
}
