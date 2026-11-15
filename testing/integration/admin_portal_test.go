// Covers: internal/app GET /admin/stats on a running instance (S11 S5, D-040 counts only)
package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/internal/config"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// TestStatsCountsOnly: a running leader's GET /admin/stats (admin key, any environment) carries
// role, epoch, instance, started_at and counts that reflect the writes, and never a key or a value
// written to a sheet. Two differently shaped key sets. Black-box: the same calls would check a
// server written in another language.
func TestStatsCountsOnly(t *testing.T) {
	const adminKey = "it-admin-key"
	srv := StartLeader(t, func(c *config.Config) { c.AdminKey = adminKey; c.Environment = "staging" })
	c := Dial(t, srv.ClientAddr)
	if r, _ := c.Open(UniqueSheet(t), "pw"); !r.Success {
		t.Fatalf("open = %+v", r)
	}
	writes := map[string]string{
		"user:42:profile":       `"value-alpha-7f3e"`,
		"user:42:settings":      `"value-beta-91c2"`,
		"cfg/region/eu-west-1":  `{"secret_marker":"value-gamma-55aa"}`,
		"cfg/region/us-east-2":  `[1,2,"value-delta-0b0b"]`,
		"cfg/region/ap-south-1": `"value-epsilon-c4d4"`,
	}
	for k, v := range writes {
		if r := c.Do(protocol.Request{Action: protocol.ActionSet, Key: k, Value: []byte(v), Epoch: 1}); !r.Success {
			t.Fatalf("set %s = %+v", k, r)
		}
	}

	req, _ := http.NewRequest("GET", srv.URL+"/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/stats: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/stats: %d %s", resp.StatusCode, body)
	}
	for k, v := range writes {
		for _, needle := range []string{k, strings.Trim(v, `"`), "value-"} {
			if strings.Contains(string(body), needle) {
				t.Fatalf("stats body carries %q: %s", needle, body)
			}
		}
	}
	var out struct {
		Nodes []struct {
			Node       string                 `json:"node"`
			Role       string                 `json:"role"`
			InstanceID string                 `json:"instance_id"`
			StartedAt  string                 `json:"started_at"`
			Epoch      uint64                 `json:"epoch"`
			Stats      map[string]json.Number `json:"stats"`
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode %s: %v (every stats field must be a number)", body, err)
	}
	if len(out.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1 (no peer)", len(out.Nodes))
	}
	n := out.Nodes[0]
	if n.Role != "leader" || n.Epoch == 0 || n.StartedAt == "" || n.Stats["entries"].String() != "5" || n.Stats["sheets"].String() != "1" {
		t.Fatalf("node entry = %s", body)
	}
}
