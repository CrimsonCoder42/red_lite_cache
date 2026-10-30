// Covers: internal/session
package session

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type killRec struct {
	mu    sync.Mutex
	codes []int
}

func (k *killRec) fn(code int, _ string) {
	k.mu.Lock()
	k.codes = append(k.codes, code)
	k.mu.Unlock()
}

func TestRegistryListFilterKill(t *testing.T) {
	r := NewRegistry("node-a")
	t0 := time.Unix(1000, 0)
	p1, p2 := uuid.New(), uuid.New()
	k1, k2 := uuid.New(), uuid.New()
	var kr killRec
	a := New(Params{Project: p1, Sheet: "s1", KeyID: k1, Remote: "1.1.1.1:1", Epoch: 2, Kill: kr.fn}, t0)
	b := New(Params{Project: p1, Sheet: "s1", KeyID: k2, Kill: kr.fn}, t0.Add(time.Second))
	c := New(Params{Project: p2, Sheet: "s2", Kill: kr.fn}, t0.Add(2*time.Second))
	for _, s := range []*Session{c, a, b} {
		r.Add(s)
	}

	all := r.List(Filter{})
	if len(all) != 3 || all[0].ID != a.ID() || all[2].ID != c.ID() || all[0].Node != "node-a" {
		t.Fatalf("List order/node wrong: %+v", all)
	}
	if got := r.List(Filter{Project: p1}); len(got) != 2 {
		t.Fatalf("project filter: %d", len(got))
	}

	a.Touch(t0.Add(time.Minute))
	a.Sent()
	info := r.List(Filter{KeyID: k1})[0]
	if !info.LastSeen.Equal(t0.Add(time.Minute)) || info.FramesIn != 1 || info.FramesOut != 1 || info.Epoch != 2 || info.Remote != "1.1.1.1:1" {
		t.Fatalf("info = %+v", info)
	}

	// criterion 7's shape: revoking key k1 closes only k1's session
	if n := r.KillWhere(Filter{KeyID: k1}, 4002, "key revoked"); n != 1 {
		t.Fatalf("KillWhere key = %d", n)
	}
	if n := r.KillWhere(Filter{KeyID: k1}, 4002, "key revoked"); n != 0 {
		t.Fatalf("a killed session was killed twice: %d", n)
	}
	if n := r.KillWhere(Filter{Project: p1}, 4001, "suspended"); n != 1 {
		t.Fatalf("suspend kills the remaining p1 session only: %d", n)
	}
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if len(kr.codes) != 2 || kr.codes[0] != 4002 || kr.codes[1] != 4001 {
		t.Fatalf("kill codes = %v", kr.codes)
	}
	if !r.Remove(a.ID()) || r.Remove(a.ID()) || r.Len() != 2 {
		t.Fatal("Remove")
	}
}

func TestConcurrentTouchAndList(t *testing.T) {
	r := NewRegistry("n")
	s := New(Params{Project: uuid.New(), Sheet: "s"}, time.Now())
	r.Add(s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				s.Touch(time.Now())
				_ = r.List(Filter{})
			}
		})
	}
	wg.Wait()
	if r.List(Filter{})[0].FramesIn != 8000 {
		t.Fatal("lost touches")
	}
}
