// Covers: backendAdvo cache layer
package conformance

// The corpus self-check, C4 (prefix sweep scope over every key shape), C5 (the permissions suffix
// sweep) and C6 (invalidateDomainBoth).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestCorpusShapes checks the corpus itself: ≥40 shapes, each key starts with its root, a
// profile-scoped key starts with "<root>:patient:<uuid>:" and no other root's sweep for that
// profile reaches any key, and no two shapes spell the same key.
func TestCorpusShapes(t *testing.T) {
	if len(Corpus) < 40 {
		t.Fatalf("corpus has %d shapes, want ≥ 40", len(Corpus))
	}
	p, tn := uuid.New(), uuid.New()
	roots := Roots()
	owner := map[string]string{}
	domains := map[string]int{}
	for _, s := range Corpus {
		domains[s.Domain()]++
		if s.Builders == "" || !strings.HasPrefix(s.Builders, "keys.go:") {
			t.Errorf("%s: Builders %q must cite keys.go:LINE", s.Name, s.Builders)
		}
		for seed := range 4 {
			k := s.Sample(p, tn, seed)
			if !strings.HasPrefix(k, s.Root+":") {
				t.Errorf("%s: %q does not start with its root %q", s.Name, k, s.Root)
			}
			for _, r := range roots {
				reached := strings.HasPrefix(k, DomainPatientPrefix(r, p))
				if want := s.ProfileScoped && r == s.Root; reached != want {
					t.Errorf("%s: %q reached by sweep (%s, p) = %v, want %v", s.Name, k, r, reached, want)
				}
			}
			if prev, dup := owner[k]; dup && prev != s.Name {
				t.Errorf("%q spelled by both %s and %s", k, prev, s.Name)
			}
			owner[k] = s.Name
		}
	}
	t.Logf("corpus: %d shapes, %d profile roots, by domain %v", len(Corpus), len(roots), domains)
}

// cfCorpusKeys is the corpus for three profiles and one tenant, deduplicated, with which
// profile's sweep of which root may remove each key ("" root = no profile sweep may).
type cfKey struct {
	key     string
	root    string // "" unless profile-scoped
	profile uuid.UUID
}

func cfCorpusKeys(tenant uuid.UUID, profiles ...uuid.UUID) ([]string, []cfKey) {
	seen := map[string]bool{}
	var keys []string
	var meta []cfKey
	for _, p := range profiles {
		for _, ck := range KeysFor(p, tenant, 2) {
			if seen[ck.Key] {
				continue
			}
			seen[ck.Key] = true
			keys = append(keys, ck.Key)
			m := cfKey{key: ck.Key}
			if ck.Shape.ProfileScoped {
				m.root, m.profile = ck.Shape.Root, p
			}
			meta = append(meta, m)
		}
	}
	return keys, meta
}

// TestC4PrefixSweepScope: the whole corpus for three profiles on all three instances; for every
// root, InvalidateDomain(root, p1) removes exactly p1's keys of that root on every instance —
// p2's and p3's keys, other roots and the profile-independent keys (records, users, catalogs)
// stay.
func TestC4PrefixSweepScope(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		p1, p2, p3, tn := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		keys, meta := cfCorpusKeys(tn, p1, p2, p3)
		c.putAll(keys)

		gone := map[string]bool{}
		for i, root := range Roots() {
			if err := c.st[i%3].InvalidateDomain(context.Background(), root, p1); err != nil {
				t.Fatalf("InvalidateDomain(%s): %v", root, err)
			}
			for _, k := range meta {
				if k.root == root && k.profile == p1 {
					gone[k.key] = true
				}
			}
			c.expectPresence(keys, gone, "after sweeping ("+root+", p1)")
		}
		n := 0
		for _, k := range meta {
			if k.root != "" && k.profile == p1 {
				n++
			}
		}
		if len(gone) != n || n == 0 {
			t.Fatalf("removed %d keys, p1 has %d profile-scoped keys", len(gone), n)
		}
		t.Logf("C4: %d keys on the sheet, %d removed by %d sweeps, %d untouched", len(keys), len(gone), len(Roots()), len(keys)-len(gone))
	})
}

// TestC5SuffixSweep: DeleteSuffix(":permissions") removes user:<uuid>:<tenant>:permissions in
// every tenant on every instance; :profile, :record and every other corpus key stay.
// after backendAdvo/cache/permissions.go:132 (s.DeleteSuffix(ctx, userPermissionsSuffix)).
func TestC5SuffixSweep(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		u := uuid.New()
		tenants := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
		keys, _ := cfCorpusKeys(tenants[0], uuid.New())
		var perms []string
		for _, tn := range tenants {
			perms = append(perms, UserPermissionsKey(u, tn))
			keys = append(keys, UserPermissionsKey(u, tn), UserProfileKey(u, tn), UserRecordKey(u, tn))
		}
		c.putAll(keys)

		// The consumer miniature has no DeleteSuffix; backendAdvo's Store.DeleteSuffix is a pass-through.
		if err := c.st[1].R.DeleteSuffix(context.Background(), UserPermissionsSuffix); err != nil {
			t.Fatalf("DeleteSuffix: %v", err)
		}
		gone := map[string]bool{}
		for _, k := range keys {
			if strings.HasSuffix(k, UserPermissionsSuffix) {
				gone[k] = true
			}
		}
		for _, k := range perms {
			if !gone[k] {
				t.Fatalf("%s does not end with %s", k, UserPermissionsSuffix)
			}
		}
		c.expectPresence(keys, gone, "after DeleteSuffix(:permissions)")
	})
}

// TestC6BothProfiles: InvalidateDomainBoth sweeps the owner and a different requester; a uuid.Nil
// requester is skipped (no error, nothing else removed); requester == owner sweeps once.
func TestC6BothProfiles(t *testing.T) {
	cfForModes(t, func(t *testing.T, m cfMode) {
		c := cfStart(t, m)
		ctx := context.Background()
		owner, requester, bystander, alone, self, tn := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
		keys, meta := cfCorpusKeys(tn, owner, requester, bystander, alone, self)
		c.putAll(keys)
		gone := map[string]bool{}
		drop := func(ps ...uuid.UUID) {
			for _, k := range meta {
				for _, p := range ps {
					if k.root == "condition" && k.profile == p {
						gone[k.key] = true
					}
				}
			}
		}

		if err := c.st[0].InvalidateDomainBoth(ctx, "condition", owner, requester); err != nil {
			t.Fatalf("both(owner, requester): %v", err)
		}
		drop(owner, requester)
		c.expectPresence(keys, gone, "owner + requester")

		if err := c.st[1].InvalidateDomainBoth(ctx, "condition", alone, uuid.Nil); err != nil {
			t.Fatalf("both(alone, Nil): %v — a Nil requester must be skipped, not refused", err)
		}
		drop(alone)
		c.expectPresence(keys, gone, "Nil requester skipped")

		if err := c.st[2].InvalidateDomainBoth(ctx, "condition", self, self); err != nil {
			t.Fatalf("both(self, self): %v", err)
		}
		drop(self)
		c.expectPresence(keys, gone, "requester == owner")

		if err := c.st[0].InvalidateDomainBoth(ctx, "condition", uuid.Nil, owner); !errors.Is(err, ErrEmptyPrefix) {
			t.Fatalf("both(Nil owner) = %v, want ErrEmptyPrefix", err)
		}
		c.expectPresence(keys, gone, "Nil owner refused")
	})
}
