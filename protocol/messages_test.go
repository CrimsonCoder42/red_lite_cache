// Covers: protocol
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestV1CompatibilityFieldNames encodes every frame with every field set and asserts each name on
// the doc/protocol/wire.md v1 compatibility list is still emitted under that exact name. A rename
// of any of them fails here before it can reach a shipped reader.
func TestV1CompatibilityFieldNames(t *testing.T) {
	seq := uint64(0)
	frames := []any{
		Request{Action: "a", Sheet: "s", Password: "p", Key: "k", Value: json.RawMessage(`1`), TTL: 1,
			Prefix: "p", Suffix: "s", Jitter: true, LockTTL: 1, InstanceID: "i", StartedAt: "t", BaseSeq: &seq},
		Response{Success: true, Error: "e", Sheet: "s", Key: "k", Value: json.RawMessage(`1`), Deleted: 1, Count: 1},
		SheetState{Type: TypeSheetState, Data: map[string]json.RawMessage{"k": json.RawMessage(`1`)}},
		Replicate{Type: TypeReplicate, Sheet: "s", Key: "k", Value: json.RawMessage(`1`)},
		Invalidate{Type: TypeInvalidate, Key: "k", Prefix: Ptr("p"), Suffix: Ptr("s")},
	}
	var all strings.Builder
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal %T: %v", f, err)
		}
		all.Write(b)
	}
	// sheets, owner: leader → v1 replica frames (full_state, replicate_lock) no longer sent; see wire.md.
	for _, name := range []string{"action", "type", "success", "error", "sheet", "password", "key", "value",
		"ttl", "prefix", "suffix", "jitter", "lock_ttl", "deleted", "count", "data", "instance_id", "started_at"} {
		if !strings.Contains(all.String(), `"`+name+`":`) {
			t.Errorf("v1 field %q is no longer emitted", name)
		}
	}
}

// TestNewFieldsOmittedWhenZero asserts every field added since v1 is omitempty: a v1 frame
// encoded by this package is byte-identical to what advo_cache sent.
func TestNewFieldsOmittedWhenZero(t *testing.T) {
	cases := []struct {
		want  string
		frame any
	}{
		{`{"action":"set","key":"k","value":1}`, Request{Action: "set", Key: "k", Value: json.RawMessage(`1`)}},
		{`{"success":true,"key":"k"}`, Response{Success: true, Key: "k"}},
		{`{"type":"replicate","key":"k","value":1}`, Replicate{Type: TypeReplicate, Key: "k", Value: json.RawMessage(`1`)}},
		{`{"type":"invalidate","key":"k"}`, Invalidate{Type: TypeInvalidate, Key: "k"}},
		{`{"type":"sheet_state","data":{}}`, SheetState{Type: TypeSheetState, Data: map[string]json.RawMessage{}}},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.frame)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%T: got %s want %s", c.frame, b, c.want)
		}
	}
}

// TestBaseSeqZeroIsExpressible: base_seq 0 means "missed before any write" and must reach the
// server, so the field is a pointer and survives omitempty.
func TestBaseSeqZeroIsExpressible(t *testing.T) {
	zero := uint64(0)
	b, _ := json.Marshal(Request{Action: ActionSet, BaseSeq: &zero})
	if !strings.Contains(string(b), `"base_seq":0`) {
		t.Fatalf("base_seq 0 dropped: %s", b)
	}
	var r Request
	if err := json.Unmarshal(b, &r); err != nil || r.BaseSeq == nil || *r.BaseSeq != 0 {
		t.Fatalf("base_seq 0 did not round trip: %+v %v", r, err)
	}
}
