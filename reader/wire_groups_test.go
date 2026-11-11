package reader_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/protocol"
	"github.com/CrimsonCoder42/red_lite_cache/testing/integration"
)

// Black-box over the wire (S9 criterion 2, D-044): only Cache.Session messages, nothing from inside the
// server, so it runs unchanged against any implementation at an address. delete_prefix and
// delete_suffix with a pattern that ends or starts anywhere — mid-segment, at a separator, on keys
// with no separator at all — delete exactly the keys that start or end with it, on two differently
// shaped key sets.

func wireColonKeys() []string {
	var out []string
	for i := range 60 {
		out = append(out, fmt.Sprintf("t%d:owner:%04d:%s", i%3, i/3, []string{"list", "count", "detail"}[i%3]))
	}
	for i := range 20 {
		out = append(out, fmt.Sprintf("t%d:rec:%04d", i%3, i))
	}
	return out
}

func wirePathKeys() []string {
	var out []string
	for i := range 80 {
		out = append(out, fmt.Sprintf("org-%d/site-%02d/dev-%04d/m-%s", i%4, i%7, i/2, []string{"temp", "volt"}[i%2]))
	}
	return out
}

func TestWireGroupsAnyBoundary(t *testing.T) {
	srv := integration.StartLeader(t)
	sets := map[string][]string{"colon": wireColonKeys(), "path": wirePathKeys()}
	type pattern struct {
		prefix bool
		arg    string
	}
	cases := map[string][]pattern{
		"colon": {
			{true, "t1:owner:00"}, // mid third segment
			{true, "t1:owner:"},   // at a separator
			{true, "t2:o"},        // mid second segment
			{true, "t0:rec:0012"}, // one whole key
			{false, "nt"},         // mid last segment ("count")
			{false, ":rec:0007"},  // at a separator
			{false, "1:list"},     // across a separator
			{false, "zz"},         // nothing
		},
		"path": {
			{true, "org-2/site-0"},     // mid segment
			{true, "org-3/"},           // at a separator
			{true, "org-1/site-05/de"}, // mid third segment
			{false, "/m-volt"},         // at a separator
			{false, "lt"},              // mid last segment
			{false, "0007/m-temp"},     // across a separator
			{false, "/dev-0003/m-volt"},
		},
	}
	n := 0
	for name, keys := range sets {
		for _, p := range cases[name] {
			n++
			sheet := fmt.Sprintf("wire-groups-%d", n)
			t.Run(fmt.Sprintf("%s/prefix=%v/%q", name, p.prefix, p.arg), func(t *testing.T) {
				c, _ := integration.OpenRaw(t, srv, sheet)
				for _, k := range keys {
					if r := c.Do(protocol.Request{Action: protocol.ActionSet, Key: k, Value: json.RawMessage(`1`)}); !r.Success {
						t.Fatalf("set %q: %+v", k, r)
					}
				}
				match := func(k string) bool {
					if p.prefix {
						return strings.HasPrefix(k, p.arg)
					}
					return strings.HasSuffix(k, p.arg)
				}
				want := 0
				for _, k := range keys {
					if match(k) {
						want++
					}
				}
				req := protocol.Request{Action: protocol.ActionDeleteSuffix, Suffix: p.arg}
				if p.prefix {
					req = protocol.Request{Action: protocol.ActionDeletePrefix, Prefix: p.arg}
				}
				if r := c.Do(req); !r.Success || r.Deleted != want {
					t.Fatalf("deleted %d (success %v), want %d", r.Deleted, r.Success, want)
				}
				for _, k := range keys {
					r := c.Do(protocol.Request{Action: protocol.ActionGet, Key: k})
					if r.Success == match(k) {
						t.Fatalf("get %q after the delete: success %v, matched %v", k, r.Success, match(k))
					}
				}
			})
		}
	}
}
