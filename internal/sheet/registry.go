package sheet

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/CrimsonCoder42/red_lite_cache/internal/cache"
	"github.com/CrimsonCoder42/red_lite_cache/internal/logger"
)

// Sentinel errors. internal/ws maps each to its protocol.ErrMsg* string.
var (
	ErrSheetNotFound = errors.New("sheet: not found")
	ErrBadPassword   = errors.New("sheet: bad password")
	ErrSheetExists   = errors.New("sheet: already exists")
	ErrNameRequired  = errors.New("sheet: name and password required")
	ErrStaleWrite    = errors.New("sheet: stale write")
	ErrSheetFull     = errors.New("sheet: full")
)

// DefaultProject is the all-zero project id: the 'default' project seeded by migration 000001,
// which owns every autocreated sheet.
var DefaultProject = uuid.UUID{}

// Options configures a Registry and every sheet in it.
type Options struct {
	Policy         cache.Policy
	MaxKeys        int  // per sheet; 0 = unbounded
	Autocreate     bool // D-013: true = the first open of an unknown sheet creates it
	BcryptCost     int  // < bcrypt.MinCost uses bcrypt.DefaultCost
	SweepLogWindow time.Duration
	SweepLogMax    int
	Now            func() time.Time // nil = time.Now
}

// Registry owns every sheet of the process. A sheet lives until Remove (replica) — never until its
// last collaborator leaves (K13).
//
// after advo_cache/sheet/manager.go:14-17: sync.Map became map + RWMutex (D-002); creation is
// gated by Options.Autocreate and the admin Create (K14); bcrypt runs outside the lock.
type Registry struct {
	opts      Options
	observers atomic.Pointer[[]Observer]

	mu     sync.RWMutex
	sheets map[string]*Sheet
}

// NewRegistry returns an empty registry.
func NewRegistry(o Options) *Registry {
	if o.Now == nil {
		o.Now = time.Now
	}
	r := &Registry{opts: o, sheets: make(map[string]*Sheet)}
	r.observers.Store(&[]Observer{})
	return r
}

// AddObserver registers o for every op of every sheet. Call it before serving.
func (r *Registry) AddObserver(o Observer) {
	for {
		cur := r.observers.Load()
		next := append(append([]Observer(nil), *cur...), o)
		if r.observers.CompareAndSwap(cur, &next) {
			return
		}
	}
}

func (r *Registry) now() time.Time { return r.opts.Now() }

func (r *Registry) hash(password string) ([]byte, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), r.opts.BcryptCost)
	if err != nil {
		return nil, fmt.Errorf("sheet: hash password: %w", err)
	}
	return h, nil
}

// Open returns the named sheet when password matches. An unknown sheet is created (created=true,
// OpCreate emitted, Warn "Sheet: autocreated", owned by DefaultProject) when Options.Autocreate is
// on, else ErrSheetNotFound. bcrypt never runs under the registry lock.
//
// after advo_cache/sheet/manager.go:27-64 (GetOrCreateSheet): same load, hash, insert-or-use-theirs
// race handling, but creation is optional (K14) and an existing sheet with no hash refuses every
// password instead of adopting the first one (K12, sheet.go:66-78).
func (r *Registry) Open(name, password string) (s *Sheet, created bool, err error) {
	if name == "" || password == "" {
		return nil, false, ErrNameRequired
	}
	if s, ok := r.Get(name); ok {
		return r.check(s, password)
	}
	if !r.opts.Autocreate {
		return nil, false, fmt.Errorf("sheet: open %q: %w", name, ErrSheetNotFound)
	}
	hash, err := r.hash(password)
	if err != nil {
		return nil, false, err
	}
	s, created = r.insert(DefaultProject, name, hash)
	if !created {
		return r.check(s, password)
	}
	logger.L.Warn("Sheet: autocreated", "sheet", name, "project", DefaultProject.String())
	return s, true, nil
}

func (r *Registry) check(s *Sheet, password string) (*Sheet, bool, error) {
	if !s.CheckPassword(password) {
		return nil, false, fmt.Errorf("sheet: open %q: %w", s.name, ErrBadPassword)
	}
	return s, false, nil
}

// insert adds a new sheet unless one with that name exists (returned with created=false). A new
// sheet's mutex is held from before it becomes visible until its OpCreate is observed, so no op on
// it can reach an observer ahead of its creation.
func (r *Registry) insert(project uuid.UUID, name string, hash []byte) (*Sheet, bool) {
	s := newSheet(r, project, name, hash)
	s.mu.Lock()
	r.mu.Lock()
	if existing, ok := r.sheets[name]; ok {
		r.mu.Unlock()
		s.mu.Unlock()
		return existing, false
	}
	r.sheets[name] = s
	r.mu.Unlock()
	s.observe(Op{Kind: OpCreate, Seq: s.seq.Load(), PasswordHash: hash})
	s.mu.Unlock()
	return s, true
}

// Create is the admin API's create: a new sheet owned by project. ErrSheetExists when the name is
// taken. Emits OpCreate.
func (r *Registry) Create(project uuid.UUID, name, password string) (*Sheet, error) {
	if name == "" || password == "" {
		return nil, ErrNameRequired
	}
	if _, ok := r.Get(name); ok {
		return nil, fmt.Errorf("sheet: create %q: %w", name, ErrSheetExists)
	}
	hash, err := r.hash(password)
	if err != nil {
		return nil, err
	}
	s, created := r.insert(project, name, hash)
	if !created {
		return nil, fmt.Errorf("sheet: create %q: %w", name, ErrSheetExists)
	}
	logger.L.Info("Sheet: created", "sheet", name, "project", project.String())
	return s, nil
}

// Restore creates the named sheet with passwordHash, or updates an existing sheet's hash and
// project (hydration, replica stream). The hash is kept as given, so a promoted replica
// authenticates exactly like the leader (K12). Emits nothing.
func (r *Registry) Restore(project uuid.UUID, name string, passwordHash []byte) *Sheet {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sheets[name]; ok {
		s.setIdentity(project, passwordHash)
		return s
	}
	s := newSheet(r, project, name, passwordHash)
	r.sheets[name] = s
	return s
}

// Get returns the named sheet.
func (r *Registry) Get(name string) (*Sheet, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sheets[name]
	return s, ok
}

// Sheets is a snapshot of the current sheets.
func (r *Registry) Sheets() []*Sheet {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Sheet, 0, len(r.sheets))
	for _, s := range r.sheets {
		out = append(out, s)
	}
	return out
}

// Remove drops the named sheet (replica only: absent from the leader's snapshot).
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	_, ok := r.sheets[name]
	delete(r.sheets, name)
	r.mu.Unlock()
	if ok {
		logger.L.Info("Sheet: removed", "sheet", name)
	}
}

// Drop removes the named sheet and kicks its collaborators (a revoked project, D-021): a client
// still joined to the removed sheet object would otherwise keep reading and writing it. Returns
// whether the sheet existed.
func (r *Registry) Drop(name, reason string) bool {
	r.mu.Lock()
	s, ok := r.sheets[name]
	delete(r.sheets, name)
	r.mu.Unlock()
	if !ok {
		return false
	}
	n := s.drop(reason)
	logger.L.Info("Sheet: dropped", "sheet", name, "collaborators", n, "reason", reason)
	return true
}

// Len is the number of sheets.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sheets)
}

// Stats is the registry's size summed over every sheet: counts only (D-040). Sheets are never
// evicted (D-013, D-042), so Sheets only grows by Create/Open/Restore and shrinks by Remove/Drop.
type Stats struct {
	Sheets          int
	Collaborators   int
	SweepLogEntries int
	Store           cache.Stats // summed over every sheet
}

// Stats sums every sheet's collaborators, sweep-log entries and store counts. Each sheet is read
// under its own mutex, one at a time, so the total is not one atomic snapshot of the registry.
func (r *Registry) Stats() Stats {
	sheets := r.Sheets()
	st := Stats{Sheets: len(sheets)}
	for _, s := range sheets {
		c, l, ss := s.stats()
		st.Collaborators += c
		st.SweepLogEntries += l
		st.Store.Entries += ss.Entries
		st.Store.RootBuckets += ss.RootBuckets
		st.Store.TailBuckets += ss.TailBuckets
		st.Store.HeadBuckets += ss.HeadBuckets
		st.Store.HeadRoots += ss.HeadRoots
	}
	return st
}

// ExpireAll runs one janitor pass over every sheet and returns how many keys expired.
func (r *Registry) ExpireAll(now time.Time) int {
	n := 0
	for _, s := range r.Sheets() {
		n += s.expire(now)
	}
	return n
}

// PruneSweepLogs prunes every sheet's sweep log at now without expiring any key and returns how
// many entries it dropped. The janitor runs it on a replica, where ExpireAll is skipped (the replica
// applies the leader's replicate_expire) but ApplyReplicated still fills the logs.
func (r *Registry) PruneSweepLogs(now time.Time) int {
	n := 0
	for _, s := range r.Sheets() {
		n += s.pruneLog(now)
	}
	return n
}

// KickAll kicks every collaborator of every sheet (step-down, shutdown).
func (r *Registry) KickAll(reason string) {
	n := 0
	for _, s := range r.Sheets() {
		n += s.kickAll(reason)
	}
	logger.L.Info("Sheet: kicked all", "collaborators", n, "reason", reason)
}
