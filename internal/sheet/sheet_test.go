// Covers: internal/sheet
package sheet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/protocol"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *clock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

var nextID atomic.Uint64

// fakeCollab records every frame; cap > 0 makes Send fail once cap frames are queued.
type fakeCollab struct {
	id  uint64
	cap int

	mu     sync.Mutex
	frames [][]byte
	kicked string
}

func newCollab(capacity int) *fakeCollab { return &fakeCollab{id: nextID.Add(1), cap: capacity} }

func (f *fakeCollab) ID() uint64 { return f.id }
func (f *fakeCollab) Send(frame []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kicked != "" || (f.cap > 0 && len(f.frames) >= f.cap) {
		return false
	}
	f.frames = append(f.frames, frame)
	return true
}
func (f *fakeCollab) Kick(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kicked == "" {
		f.kicked = reason
	}
}
func (f *fakeCollab) take() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.frames
	f.frames = nil
	return out
}
func (f *fakeCollab) wasKicked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kicked != ""
}

// frame is the union of every field the sheet's frames carry, for assertions.
type frame struct {
	Type        string                     `json:"type"`
	Reply       string                     `json:"reply"`
	Key         string                     `json:"key"`
	Keys        []string                   `json:"keys"`
	Prefix      *string                    `json:"prefix"`
	Suffix      *string                    `json:"suffix"`
	Value       json.RawMessage            `json:"value"`
	Seq         uint64                     `json:"seq"`
	Epoch       uint64                     `json:"epoch"`
	ExpiresInMS json.RawMessage            `json:"expires_in_ms"`
	Data        map[string]json.RawMessage `json:"data"`
}

func decode(t *testing.T, raw [][]byte) []frame {
	t.Helper()
	out := make([]frame, len(raw))
	for i, b := range raw {
		if err := json.Unmarshal(b, &out[i]); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
	}
	return out
}

type recObserver struct {
	mu  sync.Mutex
	ops []Op
}

func (o *recObserver) Observe(op Op) {
	o.mu.Lock()
	o.ops = append(o.ops, op)
	o.mu.Unlock()
}
func (o *recObserver) all() []Op {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.ops)
}

func newTestRegistry(t testing.TB, mod ...func(*Options)) (*Registry, *clock) {
	t.Helper()
	c := newClock()
	o := Options{
		Policy:         cache.Policy{DefaultTTL: 15 * time.Minute},
		Autocreate:     true,
		BcryptCost:     bcrypt.MinCost,
		SweepLogWindow: 30 * time.Second,
		SweepLogMax:    4096,
		Now:            c.Now,
	}
	for _, m := range mod {
		m(&o)
	}
	return NewRegistry(o), c
}

func openSheet(t *testing.T, r *Registry, name string) *Sheet {
	t.Helper()
	s, _, err := r.Open(name, "pw")
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	return s
}

func openReply(seq uint64) []byte { return fmt.Appendf(nil, `{"reply":"open","seq":%d}`, seq) }
func reply(r Result) []byte {
	return fmt.Appendf(nil, `{"reply":"ack","seq":%d,"err":%q}`, r.Seq, fmt.Sprint(r.Err))
}

func set(key, v string, ttl int) Mutation {
	return Mutation{Kind: OpSet, Key: key, Value: json.RawMessage(`"` + v + `"`), TTLMinutes: ttl}
}

// ── KEY loop ─────────────────────────────────────────────────────────────────

func TestKeyLoop(t *testing.T) {
	r, clk := newTestRegistry(t)
	s := openSheet(t, r, "key")
	s.Apply(nil, set("pre", "x", 0), nil)
	a, b := newCollab(0), newCollab(0)
	s.Join(a, 7, openReply)
	s.Join(b, 7, openReply)

	join := decode(t, b.take())
	if len(join) != 2 || join[0].Reply != "open" || join[0].Seq != 1 || join[1].Type != protocol.TypeSheetState ||
		join[1].Seq != 1 || join[1].Epoch != 7 || string(join[1].Data["pre"]) != `"x"` || len(join[1].ExpiresInMS) == 0 {
		t.Fatalf("join frames: %+v", join)
	}
	a.take()

	res := s.Apply(a, set("k", "v1", 1), reply)
	if res.Err != nil || res.Seq != 2 || res.ExpiresIn != time.Minute {
		t.Fatalf("set result %+v", res)
	}
	af, bf := decode(t, a.take()), decode(t, b.take())
	if len(af) != 1 || af[0].Reply != "ack" || af[0].Seq != 2 {
		t.Fatalf("sender frames %+v, want only its reply", af)
	}
	if len(bf) != 1 || bf[0].Type != protocol.TypeReplicate || bf[0].Key != "k" || string(bf[0].Value) != `"v1"` ||
		bf[0].Seq != 2 || string(bf[0].ExpiresInMS) != "60000" {
		t.Fatalf("other frames %+v", bf)
	}

	s.Apply(a, Mutation{Kind: OpDelete, Key: "k"}, reply)
	bf = decode(t, b.take())
	if len(bf) != 1 || bf[0].Type != protocol.TypeInvalidate || bf[0].Key != "k" || bf[0].Seq != 3 {
		t.Fatalf("delete broadcast %+v", bf)
	}
	if s.Has("k") {
		t.Fatal("deleted key still served")
	}

	s.Apply(b, set("k", "v2", 1), reply)
	a.take()
	b.take()
	clk.Advance(2 * time.Minute)
	if _, ok := s.Get("k"); ok {
		t.Fatal("expired key served by Get")
	}
	if n := r.ExpireAll(clk.Now()); n != 1 {
		t.Fatalf("ExpireAll = %d, want 1", n)
	}
	for _, c := range []*fakeCollab{a, b} {
		f := decode(t, c.take())
		if len(f) != 1 || f[0].Type != protocol.TypeInvalidate || !slices.Equal(f[0].Keys, []string{"k"}) || f[0].Seq != 5 {
			t.Fatalf("janitor broadcast %+v", f)
		}
	}
	if s.Seq() != 5 {
		t.Fatalf("seq %d, want 5", s.Seq())
	}
}

func TestSheetStateEmpty(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "empty")
	c := newCollab(0)
	s.Join(c, 0, nil)
	raw := c.take()
	if len(raw) != 1 || string(raw[0]) != `{"type":"sheet_state","data":{}}` {
		t.Fatalf("empty sheet_state = %s", raw)
	}
}

func TestSetNull(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "null")
	s.Apply(nil, Mutation{Kind: OpSet, Key: "n", Value: protocol.NullValue}, nil)
	e, ok := s.Get("n")
	if !ok || string(e.Value) != `"__NULL__"` {
		t.Fatalf("set_null stored %s %v", e.Value, ok)
	}
}

// ── SWEEP loop ───────────────────────────────────────────────────────────────

func TestSweepLoop(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    Mutation
		in   []string
		out  string
	}{
		{"prefix", Mutation{Kind: OpDeletePrefix, Prefix: "t:patient:1:"}, []string{"t:patient:1:a", "t:patient:1:b", "t:patient:1:c"}, "t:patient:2:a"},
		{"suffix", Mutation{Kind: OpDeleteSuffix, Suffix: ":permissions"}, []string{"t:1:permissions", "t:2:permissions", "u:permissions"}, "t:1:view"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRegistry(t)
			s := openSheet(t, r, "sweep-"+tc.name)
			for _, k := range append(slices.Clone(tc.in), tc.out) {
				s.Apply(nil, set(k, "v", 0), nil)
			}
			other := newCollab(0)
			s.Join(other, 0, nil)
			other.take()
			res := s.Apply(nil, tc.m, nil)
			if res.Deleted != 3 {
				t.Fatalf("deleted %d, want 3", res.Deleted)
			}
			for _, k := range tc.in {
				if s.Has(k) {
					t.Fatalf("%s survived the sweep", k)
				}
			}
			if !s.Has(tc.out) {
				t.Fatalf("%s outside the sweep was deleted", tc.out)
			}
			f := decode(t, other.take())
			if len(f) != 1 || f[0].Type != protocol.TypeInvalidate || f[0].Seq != res.Seq {
				t.Fatalf("sweep broadcast %+v", f)
			}
			if (tc.m.Kind == OpDeletePrefix && (f[0].Prefix == nil || *f[0].Prefix != tc.m.Prefix)) ||
				(tc.m.Kind == OpDeleteSuffix && (f[0].Suffix == nil || *f[0].Suffix != tc.m.Suffix)) {
				t.Fatalf("sweep broadcast pattern %+v", f[0])
			}
		})
	}
}

func TestDeletePrefixEmptyBroadcastsPrefix(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "flush")
	s.Apply(nil, set("a", "1", 0), nil)
	c := newCollab(0)
	s.Join(c, 0, nil)
	c.take()
	s.Apply(nil, Mutation{Kind: OpDeletePrefix, Prefix: ""}, nil)
	raw := c.take()
	if len(raw) != 1 || string(raw[0]) != `{"type":"invalidate","prefix":"","seq":2}` || s.Len() != 0 {
		t.Fatalf("flush frame %s len %d", raw, s.Len())
	}
}

// ── ORDER ────────────────────────────────────────────────────────────────────

func TestOrderConcurrentApply(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "order")
	obs := &recObserver{}
	r.AddObserver(obs)
	collabs := []*fakeCollab{newCollab(0), newCollab(0), newCollab(0)}
	for _, c := range collabs {
		s.Join(c, 0, nil)
		c.take()
	}
	const writers, each = 8, 200
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			sender := collabs[w%len(collabs)]
			for i := range each {
				m := set(fmt.Sprintf("w%d:%d", w, i%10), "v", 0)
				if i%5 == 0 {
					m = Mutation{Kind: OpDeletePrefix, Prefix: fmt.Sprintf("w%d:", w)}
				}
				s.Apply(sender, m, reply)
			}
		})
	}
	wg.Wait()
	total := uint64(writers * each)
	for i, c := range collabs {
		var last uint64
		frames := decode(t, c.take())
		for _, f := range frames {
			if f.Seq <= last {
				t.Fatalf("collab %d: seq %d after %d", i, f.Seq, last)
			}
			last = f.Seq
		}
		if last != total || len(frames) != int(total) {
			t.Fatalf("collab %d: %d frames ending at seq %d, want %d", i, len(frames), last, total)
		}
	}
	ops := obs.all()
	if len(ops) != int(total) {
		t.Fatalf("observer saw %d ops, want %d", len(ops), total)
	}
	for i, op := range ops {
		if op.Seq != uint64(i+1) || op.Sheet != "order" {
			t.Fatalf("observer op %d: %+v", i, op)
		}
	}
}

// Every broadcast after Join reaches the joiner after its sheet_state.
func TestJoinOrderUnderLoad(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "joinorder")
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ctx.Err() == nil; i++ {
			s.Apply(nil, set(fmt.Sprintf("k%d", i%50), "v", 0), nil)
		}
	})
	for range 50 {
		c := newCollab(0)
		s.Join(c, 0, openReply)
		time.Sleep(50 * time.Microsecond)
		s.Leave(c)
		f := decode(t, c.take())
		if len(f) < 2 || f[0].Reply != "open" || f[1].Type != protocol.TypeSheetState || f[0].Seq != f[1].Seq {
			t.Fatalf("join prefix %+v", f[:min(2, len(f))])
		}
		for _, x := range f[2:] {
			if x.Seq != f[1].Seq+1 {
				t.Fatalf("broadcast seq %d after state seq %d", x.Seq, f[1].Seq)
			}
			f[1].Seq = x.Seq
		}
	}
	cancel()
	wg.Wait()
}

// ── BACKPRESSURE ─────────────────────────────────────────────────────────────

func TestBackpressureKicks(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "bp")
	slow, ok1, ok2 := newCollab(2), newCollab(0), newCollab(0) // slow: room for the state frame + 1
	for _, c := range []*fakeCollab{slow, ok1, ok2} {
		s.Join(c, 0, nil)
	}
	for i := range 5 {
		s.Apply(nil, set(fmt.Sprintf("k%d", i), "v", 0), nil)
	}
	if !slow.wasKicked() {
		t.Fatal("collaborator with a full queue was not kicked")
	}
	if n := s.Leave(slow); n != 2 {
		t.Fatalf("collaborators after kick = %d, want 2 (kicked one already dropped)", n)
	}
	for _, c := range []*fakeCollab{ok1, ok2} {
		if c.wasKicked() || len(c.take()) != 6 {
			t.Fatal("healthy collaborator affected by the slow one")
		}
	}
	if s.Len() != 5 {
		t.Fatalf("sheet stopped serving: len %d", s.Len())
	}
}

func TestSenderReplyFullKicksSender(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "bp-sender")
	sender := newCollab(1)
	s.Join(sender, 0, nil) // fills its one slot
	res := s.Apply(sender, set("k", "v", 0), reply)
	if res.Err != nil || !sender.wasKicked() || !s.Has("k") {
		t.Fatalf("res %+v kicked %v", res, sender.wasKicked())
	}
}

// ── R6 sweep log ─────────────────────────────────────────────────────────────

func ptr(v uint64) *uint64 { return &v }

func TestStaleWrite(t *testing.T) {
	r, clk := newTestRegistry(t, func(o *Options) { o.SweepLogMax = 4 })
	s := openSheet(t, r, "r6")
	obs := &recObserver{}
	r.AddObserver(obs)
	s.Apply(nil, set("t:patient:1:view", "old", 0), nil)                      // seq 1
	base := s.Seq()                                                           // reader missed another key at seq 1
	s.Apply(nil, Mutation{Kind: OpDeletePrefix, Prefix: "t:patient:1:"}, nil) // seq 2

	stale := set("t:patient:1:list", "fromdb", 0)
	stale.BaseSeq = ptr(base)
	var replied Result
	res := s.Apply(newCollab(0), stale, func(r Result) []byte { replied = r; return []byte(`{}`) })
	if !errors.Is(res.Err, ErrStaleWrite) || s.Seq() != 2 || s.Has("t:patient:1:list") || res.Seq != 2 || !errors.Is(replied.Err, ErrStaleWrite) {
		t.Fatalf("stale set applied: %+v seq %d", res, s.Seq())
	}
	if n := len(obs.all()); n != 2 { // the set and the sweep, not the refused write
		t.Fatalf("observer saw %d ops from a refused write", n)
	}

	uncovered := set("t:patient:2:list", "fromdb", 0)
	uncovered.BaseSeq = ptr(base)
	if res := s.Apply(nil, uncovered, nil); res.Err != nil {
		t.Fatalf("set not covered by the sweep refused: %v", res.Err)
	}

	fresh := set("t:patient:1:list", "fromdb", 0)
	fresh.BaseSeq = ptr(2)
	if res := s.Apply(nil, fresh, nil); res.Err != nil || !s.Has("t:patient:1:list") {
		t.Fatalf("fresh set refused: %+v", res)
	}

	s.Apply(nil, set("t:patient:1:list", "again", 0), nil) // a set also covers its key
	later := set("t:patient:1:list", "x", 0)
	later.BaseSeq = ptr(4)
	if res := s.Apply(nil, later, nil); !errors.Is(res.Err, ErrStaleWrite) {
		t.Fatalf("set older than a later set accepted: %+v", res)
	}

	// Push the log past SweepLogMax: floor rises, a base below it is stale even for an untouched key.
	for i := range 6 {
		s.Apply(nil, set(fmt.Sprintf("other%d", i), "v", 0), nil)
	}
	floorTest := set("never:touched", "x", 0)
	floorTest.BaseSeq = ptr(1)
	if res := s.Apply(nil, floorTest, nil); !errors.Is(res.Err, ErrStaleWrite) {
		t.Fatalf("base below floor accepted: %+v", res)
	}

	// Window pruning: after SweepLogWindow with no writes the floor is the last seq.
	clk.Advance(time.Minute)
	cur := s.Seq()
	atCur := set("never:touched", "x", 0)
	atCur.BaseSeq = ptr(cur)
	if res := s.Apply(nil, atCur, nil); res.Err != nil {
		t.Fatalf("base at current seq refused after window: %v", res.Err)
	}
	old := set("never:touched2", "x", 0)
	old.BaseSeq = ptr(cur - 1)
	if res := s.Apply(nil, old, nil); !errors.Is(res.Err, ErrStaleWrite) {
		t.Fatalf("base below window floor accepted: %+v", res)
	}

	// nil BaseSeq is the v1 unconditional set.
	if res := s.Apply(nil, set("t:patient:1:list", "v1", 0), nil); res.Err != nil {
		t.Fatalf("unconditional set refused: %v", res.Err)
	}
}

func TestSheetFull(t *testing.T) {
	r, _ := newTestRegistry(t, func(o *Options) { o.MaxKeys = 2 })
	s := openSheet(t, r, "full")
	s.Apply(nil, set("a", "1", 0), nil)
	s.Apply(nil, set("b", "1", 0), nil)
	if res := s.Apply(nil, set("c", "1", 0), nil); !errors.Is(res.Err, ErrSheetFull) || s.Seq() != 2 {
		t.Fatalf("set past MaxKeys: %+v", res)
	}
	if res := s.Apply(nil, set("a", "2", 0), nil); res.Err != nil {
		t.Fatalf("overwrite on a full sheet refused: %v", res.Err)
	}
}

// ── lifecycle: K12 / K13 / K14, passwords ───────────────────────────────────

func TestWrongPassword(t *testing.T) {
	r, _ := newTestRegistry(t)
	openSheet(t, r, "pw")
	if _, _, err := r.Open("pw", "nope"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := r.Open("pw", ""); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("empty password: %v", err)
	}
	if _, _, err := r.Open("", "pw"); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("empty name: %v", err)
	}
}

func TestK12RestoreKeepsHash(t *testing.T) {
	leader, _ := newTestRegistry(t)
	ls := openSheet(t, leader, "k12")
	replica, _ := newTestRegistry(t, func(o *Options) { o.Autocreate = false })
	proj := uuid.New()
	rs := replica.Restore(proj, "k12", ls.PasswordHash())
	if rs.ProjectID() != proj {
		t.Fatal("project not restored")
	}
	if _, _, err := replica.Open("k12", "attacker"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("promoted replica accepted another password: %v", err)
	}
	if s, created, err := replica.Open("k12", "pw"); err != nil || created || s != rs {
		t.Fatalf("leader's password refused on replica: %v", err)
	}
	empty := replica.Restore(proj, "nohash", nil)
	if empty.CheckPassword("anything") || empty.CheckPassword("") {
		t.Fatal("sheet with no hash accepted a password (v1 adopt-first-password bug)")
	}
}

func TestK13LeaveKeepsSheet(t *testing.T) {
	r, _ := newTestRegistry(t)
	s := openSheet(t, r, "k13")
	s.Apply(nil, set("a", "1", 0), nil)
	c := newCollab(0)
	s.Join(c, 0, nil)
	if n := s.Leave(c); n != 0 {
		t.Fatalf("remaining %d", n)
	}
	got, ok := r.Get("k13")
	if !ok || got != s || !got.Has("a") {
		t.Fatal("sheet or its data deleted when the last collaborator left")
	}
}

func TestK14Autocreate(t *testing.T) {
	off, _ := newTestRegistry(t, func(o *Options) { o.Autocreate = false })
	if _, _, err := off.Open("new", "pw"); !errors.Is(err, ErrSheetNotFound) || off.Len() != 0 {
		t.Fatalf("autocreate off: %v", err)
	}
	proj := uuid.New()
	if _, err := off.Create(proj, "admin", "pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := off.Create(proj, "admin", "pw"); !errors.Is(err, ErrSheetExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	if s, created, err := off.Open("admin", "pw"); err != nil || created || s.ProjectID() != proj {
		t.Fatalf("open of an admin-created sheet: %v %v", created, err)
	}

	on, _ := newTestRegistry(t)
	obs := &recObserver{}
	on.AddObserver(obs)
	s, created, err := on.Open("auto", "pw")
	if err != nil || !created || s.ProjectID() != DefaultProject {
		t.Fatalf("autocreate on: %v %v", created, err)
	}
	if _, created, _ := on.Open("auto", "pw"); created {
		t.Fatal("second open reported created")
	}
	ops := obs.all()
	if len(ops) != 1 || ops[0].Kind != OpCreate || ops[0].Sheet != "auto" || ops[0].ProjectID != DefaultProject ||
		bcrypt.CompareHashAndPassword(ops[0].PasswordHash, []byte("pw")) != nil {
		t.Fatalf("OpCreate %+v", ops)
	}
}

func TestConcurrentAutocreateOneWinner(t *testing.T) {
	r, _ := newTestRegistry(t)
	obs := &recObserver{}
	r.AddObserver(obs)
	var created atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			s, c, err := r.Open("race", "pw")
			if err != nil {
				t.Error(err)
				return
			}
			if c {
				created.Add(1)
			}
			s.Apply(nil, set("k", "v", 0), nil)
		})
	}
	wg.Wait()
	ops := obs.all()
	if created.Load() != 1 || r.Len() != 1 || len(ops) != 17 || ops[0].Kind != OpCreate {
		t.Fatalf("created %d sheets %d ops %d first %v", created.Load(), r.Len(), len(ops), ops[0].Kind)
	}
}

// ── observers, replication, janitor ─────────────────────────────────────────

func TestObserversSeeEveryOp(t *testing.T) {
	r, clk := newTestRegistry(t)
	obs := &recObserver{}
	r.AddObserver(obs)
	s := openSheet(t, r, "obs")
	s.Apply(nil, set("a:1", "v", 1), nil)
	s.Apply(nil, Mutation{Kind: OpDelete, Key: "a:1"}, nil)
	s.Apply(nil, set("a:2", "v", 1), nil)
	s.Apply(nil, Mutation{Kind: OpDeleteSuffix, Suffix: ":x"}, nil)
	clk.Advance(2 * time.Minute)
	r.ExpireAll(clk.Now())
	want := []OpKind{OpCreate, OpSet, OpDelete, OpSet, OpDeleteSuffix, OpExpire}
	ops := obs.all()
	if len(ops) != len(want) {
		t.Fatalf("ops %+v", ops)
	}
	for i, op := range ops {
		if op.Kind != want[i] || op.Seq != uint64(i) {
			t.Fatalf("op %d = %s seq %d, want %s seq %d", i, op.Kind, op.Seq, want[i], i)
		}
	}
	if !slices.Equal(ops[5].Keys, []string{"a:2"}) || ops[1].ExpiresAt.IsZero() {
		t.Fatalf("expire keys %v / set expiry %v", ops[5].Keys, ops[1].ExpiresAt)
	}
}

func TestApplyReplicatedAndReplace(t *testing.T) {
	leader, _ := newTestRegistry(t)
	ls := openSheet(t, leader, "rep")
	ls.Apply(nil, set("a", "1", 0), nil)
	ls.Apply(nil, set("b", "2", 0), nil)

	replica, _ := newTestRegistry(t)
	rs := replica.Restore(ls.ProjectID(), "rep", nil)
	ls.Snapshot(func(snap Snapshot) { rs.Replace(snap) })
	if rs.Seq() != 2 || !rs.Has("a") || !rs.Has("b") || !rs.CheckPassword("pw") {
		t.Fatal("Replace did not carry entries, seq and hash")
	}
	obs := &recObserver{}
	replica.AddObserver(obs)
	rs.ApplyReplicated(Op{Kind: OpDelete, Key: "a", Seq: 2}) // already applied: ignored
	if !rs.Has("a") {
		t.Fatal("old op re-applied")
	}
	rs.ApplyReplicated(Op{Kind: OpDelete, Key: "a", Seq: 3})
	rs.ApplyReplicated(Op{Kind: OpSet, Key: "c", Value: json.RawMessage(`3`), Seq: 4})
	rs.ApplyReplicated(Op{Kind: OpExpire, Keys: []string{"b"}, Seq: 5})
	if rs.Has("a") || rs.Has("b") || !rs.Has("c") || rs.Seq() != 5 || len(obs.all()) != 3 {
		t.Fatalf("replicated ops: seq %d ops %d", rs.Seq(), len(obs.all()))
	}
	stale := set("a", "x", 0)
	stale.BaseSeq = ptr(2)
	if res := rs.Apply(nil, stale, nil); !errors.Is(res.Err, ErrStaleWrite) {
		t.Fatal("replicated delete not in the R6 log")
	}
	replica.Remove("rep")
	if _, ok := replica.Get("rep"); ok || replica.Len() != 0 {
		t.Fatal("Remove kept the sheet")
	}
}

func TestRunJanitor(t *testing.T) {
	r, clk := newTestRegistry(t)
	s := openSheet(t, r, "jan")
	s.Apply(nil, set("k", "v", 1), nil)
	clk.Advance(2 * time.Minute)

	var active atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- RunJanitor(ctx, r, time.Millisecond, active.Load) }()
	time.Sleep(20 * time.Millisecond)
	if s.Len() != 1 {
		t.Fatal("inactive janitor expired keys")
	}
	active.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for s.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Len() != 0 {
		t.Fatal("active janitor did not expire")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunJanitor returned %v on ctx.Done", err)
	}
}

func TestKickAll(t *testing.T) {
	r, _ := newTestRegistry(t)
	a, b := newCollab(0), newCollab(0)
	openSheet(t, r, "s1").Join(a, 0, nil)
	openSheet(t, r, "s2").Join(b, 0, nil)
	r.KickAll("step down")
	if !a.wasKicked() || !b.wasKicked() || r.Len() != 2 {
		t.Fatal("KickAll")
	}
}

// TestDropRefusesHeldSheet (second review, F): a session that already holds a sheet when its
// project is revoked can neither write it (no apply, no observer op reaches the persist writer)
// nor join it.
func TestDropRefusesHeldSheet(t *testing.T) {
	r, _ := newTestRegistry(t)
	obs := &recObserver{}
	r.AddObserver(obs)
	s := openSheet(t, r, "revoked")
	a := newCollab(0)
	s.Join(a, 0, nil)
	before := len(obs.ops)
	if !r.Drop("revoked", "project revoked") || !a.wasKicked() {
		t.Fatal("Drop did not remove the sheet or kick its collaborator")
	}
	res := s.Apply(nil, set("k", `1`, 0), reply)
	if !errors.Is(res.Err, ErrSheetNotFound) || s.Has("k") || len(obs.ops) != before {
		t.Fatalf("apply on a dropped sheet: err=%v has=%v observed=%d→%d", res.Err, s.Has("k"), before, len(obs.ops))
	}
	late := newCollab(0)
	s.Join(late, 0, nil)
	if !late.wasKicked() {
		t.Fatal("joined a dropped sheet")
	}
}
