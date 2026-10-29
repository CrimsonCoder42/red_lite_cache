// Covers: delete_prefix, delete_suffix
package integration

import (
	"context"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/reader"
)

// sweepLoop sets three keys matching a pattern and one outside it on A, sweeps with sweep(A), and
// checks every reader (and the server) dropped exactly the three.
func sweepLoop(t *testing.T, match []string, keep string, sweep func(*reader.Reader) error) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	a := NewReader(t, srv, name)
	b := NewReader(t, srv, name)
	ctx := context.Background()
	for _, k := range append(append([]string(nil), match...), keep) {
		if err := a.Set(ctx, k, k); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	Eventually(t, "B sees all four keys", func() bool {
		for _, k := range append(append([]string(nil), match...), keep) {
			if !b.Has(k) {
				return false
			}
		}
		return true
	})
	if err := sweep(a); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, r := range []*reader.Reader{a, b} {
		Eventually(t, "the three swept, the fourth kept", func() bool {
			for _, k := range match {
				if r.Has(k) {
					return false
				}
			}
			return r.Has(keep)
		})
	}
	raw, st := OpenRaw(t, srv, name)
	if len(st.Data) != 1 || st.Data[keep] == nil {
		t.Fatalf("server sheet_state after sweep = %v, want only %s", st.Data, keep)
	}
	if g := raw.Do(protocol.Request{Action: protocol.ActionGet, Key: match[0]}); g.Error != protocol.ErrMsgKeyNotFound {
		t.Fatalf("get %s = %+v", match[0], g)
	}
}

func TestSweepLoopPrefix(t *testing.T) {
	sweepLoop(t, []string{"user:1:a", "user:1:b", "user:1:c"}, "user:2:a", func(r *reader.Reader) error {
		return r.DeletePrefix(context.Background(), "user:1:")
	})
}

func TestSweepLoopSuffix(t *testing.T) {
	sweepLoop(t, []string{"a:list", "b:list", "c:list"}, "a:item", func(r *reader.Reader) error {
		return r.DeleteSuffix(context.Background(), ":list")
	})
}

// TestSweepReplyShape checks the delete_prefix/delete_suffix replies and broadcasts on the wire:
// {success, id, deleted, seq} and invalidate{prefix|suffix, seq}, with "" emitted.
func TestSweepReplyShape(t *testing.T) {
	srv := StartLeader(t)
	name := UniqueSheet(t)
	w, _ := OpenRaw(t, srv, name)
	o, _ := OpenRaw(t, srv, name)
	for _, k := range []string{"p:1", "p:2", "x"} {
		if r := w.Do(protocol.Request{Action: protocol.ActionSet, Key: k, Value: []byte(`1`)}); !r.Success {
			t.Fatalf("set %s: %+v", k, r)
		}
	}
	r := w.Do(protocol.Request{Action: protocol.ActionDeletePrefix, ID: 9, Prefix: "p:"})
	if !r.Success || r.ID != 9 || r.Deleted != 2 || r.Seq != 4 {
		t.Fatalf("delete_prefix reply = %+v, want deleted 2 seq 4", r)
	}
	var inv protocol.Invalidate
	o.NextType(protocol.TypeInvalidate, &inv)
	if inv.Prefix == nil || *inv.Prefix != "p:" || inv.Seq != 4 {
		t.Fatalf("invalidate = %+v", inv)
	}
	r = w.Do(protocol.Request{Action: protocol.ActionDeleteSuffix, Suffix: ""})
	if !r.Success || r.Deleted != 1 || r.Seq != 5 {
		t.Fatalf("delete_suffix \"\" reply = %+v", r)
	}
	o.NextType(protocol.TypeInvalidate, &inv)
	if inv.Suffix == nil || *inv.Suffix != "" {
		t.Fatalf("invalidate suffix \"\" = %+v, want suffix emitted as \"\"", inv)
	}
}
