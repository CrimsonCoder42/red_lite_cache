// Covers: internal/tenant (D-024 admission, D-027 suspend, D-025 revoked names)
package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fakeKeys map[string][2]uuid.UUID // string(hash) → {keyID, project}

func (f fakeKeys) ResolveKey(_ context.Context, hash []byte) (uuid.UUID, uuid.UUID, error) {
	v, ok := f[string(hash)]
	if !ok {
		return uuid.Nil, uuid.Nil, ErrUnknownKey
	}
	return v[0], v[1], nil
}

func TestAdmit(t *testing.T) {
	pA, pB := uuid.New(), uuid.New()
	kA, kB := uuid.New(), uuid.New()
	keys := fakeKeys{string(HashKey("rlk_a")): {kA, pA}, string(HashKey("rlk_b")): {kB, pB}}
	ctx := context.Background()

	pw := New(keys, false)
	if id, err := pw.Admit(ctx, pA, ""); err != nil || id != uuid.Nil {
		t.Fatalf("password mode: %v %v", id, err)
	}

	d := New(keys, true)
	var touched []uuid.UUID
	d.OnAdmit(func(k uuid.UUID) { touched = append(touched, k) })
	for _, c := range []struct {
		key  string
		want error
	}{{"", ErrNotAuthorized}, {"rlk_b", ErrNotAuthorized}, {"rlk_unknown", ErrNotAuthorized}} {
		if _, err := d.Admit(ctx, pA, c.key); !errors.Is(err, c.want) {
			t.Errorf("key %q: %v, want %v", c.key, err, c.want)
		}
	}
	if id, err := d.Admit(ctx, pA, "rlk_a"); err != nil || id != kA {
		t.Fatalf("right key: %v %v", id, err)
	}
	if len(touched) != 1 || touched[0] != kA {
		t.Fatalf("OnAdmit = %v", touched)
	}

	d.SetSuspended(pA, true)
	pw.SetSuspended(pA, true)
	if _, err := d.Admit(ctx, pA, "rlk_a"); !errors.Is(err, ErrSuspended) {
		t.Fatalf("suspended key mode: %v", err)
	}
	if _, err := pw.Admit(ctx, pA, ""); !errors.Is(err, ErrSuspended) {
		t.Fatalf("suspended password mode: %v", err)
	}
	// no oracle: without a valid key of the project, a suspended project looks like any refusal
	for _, k := range []string{"", "rlk_b", "rlk_unknown"} {
		if _, err := d.Admit(ctx, pA, k); !errors.Is(err, ErrNotAuthorized) {
			t.Fatalf("suspended project, key %q: %v, want ErrNotAuthorized", k, err)
		}
	}
	d.ReplaceSuspended(nil)
	if _, err := d.Admit(ctx, pA, "rlk_a"); err != nil {
		t.Fatalf("resumed: %v", err)
	}

	d.MarkRevoked("gone")
	if !d.Revoked("gone") || d.Revoked("kept") {
		t.Fatal("revoked names")
	}
}

// TestRevokedKeyRefused (tenancy review finding 1): a key marked revoked on this node is refused
// even while the store still resolves it (the open-vs-revoke race), and KeyRevoked reports it so
// an open that raced the revoke closes its own session.
func TestRevokedKeyRefused(t *testing.T) {
	p, k := uuid.New(), uuid.New()
	d := New(fakeKeys{string(HashKey("rlk_a")): {k, p}}, true)
	if _, err := d.Admit(context.Background(), p, "rlk_a"); err != nil {
		t.Fatalf("live key: %v", err)
	}
	d.MarkKeyRevoked(k)
	if _, err := d.Admit(context.Background(), p, "rlk_a"); !errors.Is(err, ErrNotAuthorized) || !d.KeyRevoked(k) {
		t.Fatalf("revoked key: %v (revoked=%v)", err, d.KeyRevoked(k))
	}
}
