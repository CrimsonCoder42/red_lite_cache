// Covers: internal/sheet
package sheet

import (
	"fmt"
	"io"
	"testing"

	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// discardCollab accepts every frame and keeps nothing, so the benchmark measures the sheet.
type discardCollab struct{ id uint64 }

func (d discardCollab) ID() uint64       { return d.id }
func (d discardCollab) Send([]byte) bool { return true }
func (d discardCollab) Kick(string)      {}

// BenchmarkApplySet3Collaborators: one set encoded once and enqueued to 2 others + the sender's reply.
func BenchmarkApplySet3Collaborators(b *testing.B) {
	prev := logger.L
	logger.L = logger.New(io.Discard, 0, false)
	b.Cleanup(func() { logger.L = prev })
	r, _ := newTestRegistry(b)
	s, _, err := r.Open("bench", "pw")
	if err != nil {
		b.Fatal(err)
	}
	cs := []discardCollab{{nextID.Add(1)}, {nextID.Add(1)}, {nextID.Add(1)}}
	for _, c := range cs {
		s.Join(c, 0, nil)
	}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprintf("root:patient:%d:view", i)
	}
	ack := []byte(`{"success":true}`)
	i := 0
	for b.Loop() {
		s.Apply(cs[0], set(keys[i%len(keys)], "v", 0), func(Result) []byte { return ack })
		i++
	}
}
