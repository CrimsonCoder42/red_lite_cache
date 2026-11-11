// Covers: memory and goroutine bounds

package leaks

import (
	"fmt"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// L13 (S2, doc/reader_optimize_2026-11-10; nodes since D-044): the reader's local index drains with
// its map. A real leader broadcasts 6 000 scoped keys (`<root>:owner:<id>:<view>`) to a reader; both
// index node counts rise, then the keys are removed by the four broadcast shapes — exact delete,
// delete_prefix by owner, delete_prefix by root, delete_suffix — and every count returns to 0 with
// the map.
func TestL13ReaderIndexDrains(t *testing.T) {
	const profiles = 150 // × 8 roots × 5 views = 6 000 keys
	roots := []string{"condition", "med", "vitals", "episode", "pef", "allergy", "lab", "note"}
	views := []string{"list", "count", "summary", "detail", "timeline"}
	s := NewSteps(t)
	srv := integration.StartLeader(t)
	sheet := integration.UniqueSheet(t)
	rd := integration.NewReader(t, srv, sheet)
	w, _ := integration.OpenRaw(t, srv, sheet)
	base := rd.Stats()
	s.Baseline("reader %+v", base)

	var keys []string
	for p := range profiles {
		for _, r := range roots {
			for _, v := range views {
				keys = append(keys, fmt.Sprintf("%s:owner:%08x-0000-4000-8000-%012x:%s", r, p, p, v))
			}
		}
	}
	var id uint64
	s.Fill("%d keys from a raw writer", len(keys))
	readerWriteBatch(t, w, keys, &id)
	integration.Eventually(t, "reader holds every key", func() bool { return rd.Stats().Entries == len(keys) })
	peak := rd.Stats()
	s.Observe("reader %+v", peak)
	Above(t, "ForwardNodes", peak.ForwardNodes, base.ForwardNodes)
	Above(t, "ReverseNodes", peak.ReverseNodes, base.ReverseNodes)

	s.Drain("exact deletes, head prefixes, root prefixes, then a suffix")
	do := func(req protocol.Request) {
		id++
		req.ID = id
		if resp := w.Do(req); !resp.Success {
			t.Fatalf("%s refused: %+v", req.Action, resp)
		}
	}
	for _, k := range keys[:200] {
		do(protocol.Request{Action: protocol.ActionDelete, Key: k})
	}
	for p := range 50 { // one profile's head in the first root, closed and open prefixes alternating
		prefix := fmt.Sprintf("%s:owner:%08x-0000-4000-8000-%012x", roots[0], p, p)
		if p%2 == 0 {
			prefix += ":"
		}
		do(protocol.Request{Action: protocol.ActionDeletePrefix, Prefix: prefix})
	}
	for _, r := range roots[:7] {
		do(protocol.Request{Action: protocol.ActionDeletePrefix, Prefix: r + ":"})
	}
	do(protocol.Request{Action: protocol.ActionDeleteSuffix, Suffix: ":list"})
	do(protocol.Request{Action: protocol.ActionDeleteSuffix, Suffix: "count"})
	do(protocol.Request{Action: protocol.ActionDeletePrefix, Prefix: roots[7]})
	integration.Eventually(t, "reader empty", func() bool { return rd.Stats().Entries == 0 })
	end := rd.Stats()
	s.Assert("reader %+v", end)
	Within(t, "Entries", end.Entries, 0, 0)
	Within(t, "ForwardNodes", end.ForwardNodes, 0, 0)
	Within(t, "ReverseNodes", end.ReverseNodes, 0, 0)
}
