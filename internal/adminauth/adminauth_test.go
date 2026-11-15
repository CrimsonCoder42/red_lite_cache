// Covers: internal/adminauth limiter cap + sweep, ClientIP (D6), password rules (S11)
package adminauth

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestLimiterMapBounded: 10× the cap in distinct keys never grows the map past MaxEntries; a new
// key is refused while every bucket is fresh; once the old ones are idle, a sweep frees room.
func TestLimiterMapBounded(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	lim := Limits{IP: Rate{R: 1, Burst: 1}, MaxEntries: 100, Idle: 10 * time.Minute}
	l := NewLimiter(lim, clk.now)
	refused := 0
	for i := range 1000 {
		if ok, _ := l.Allow(ClassIP, fmt.Sprintf("10.0.%d.%d", i/256, i%256)); !ok {
			refused++
		}
		if n := l.Len(); n > lim.MaxEntries {
			t.Fatalf("after %d keys the map holds %d > cap %d", i+1, n, lim.MaxEntries)
		}
	}
	if refused != 900 {
		t.Fatalf("refused %d new keys at the cap, want 900", refused)
	}
	clk.add(10 * time.Minute)
	if ok, _ := l.Allow(ClassIP, "192.0.2.1"); !ok {
		t.Fatal("a new key after every bucket went idle was refused (no sweep)")
	}
	if n := l.Len(); n != 1 {
		t.Fatalf("after the sweep the map holds %d, want 1", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after its context ended")
	}
}

// TestLimiterRetryAfter: a refused request reports how long until the next token, and does not
// spend it.
func TestLimiterRetryAfter(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(Limits{Login: Rate{R: 1.0 / 12, Burst: 1}, MaxEntries: 10}, clk.now)
	if ok, _ := l.Allow(ClassLogin, "a"); !ok {
		t.Fatal("first request refused")
	}
	ok, retry := l.Allow(ClassLogin, "a")
	if ok || retry < 11*time.Second || retry > 12*time.Second {
		t.Fatalf("second request: ok=%v retry=%v, want refused with ~12s", ok, retry)
	}
	clk.add(12 * time.Second)
	if ok, _ := l.Allow(ClassLogin, "a"); !ok {
		t.Fatal("refused after the refill: a refused request spent the token")
	}
	if ok, _ := l.Allow(ClassUser, "a"); !ok {
		t.Fatal("a disabled class (Burst 0) refused")
	}
}

// TestTrustedProxyOnlyFromLoopback is D6: X-Forwarded-For counts only with the flag on and only
// from a loopback peer, and only its last hop.
func TestTrustedProxyOnlyFromLoopback(t *testing.T) {
	cases := []struct {
		name, remote string
		xff          []string
		trust        bool
		want         string
	}{
		{"flag off ignores XFF", "127.0.0.1:5000", []string{"203.0.113.9"}, false, "127.0.0.1"},
		{"loopback proxy, last hop", "127.0.0.1:5000", []string{"198.51.100.1, 203.0.113.9"}, true, "203.0.113.9"},
		{"last header wins", "[::1]:5000", []string{"198.51.100.1", "203.0.113.7"}, true, "203.0.113.7"},
		{"non-loopback peer ignores XFF", "192.0.2.10:5000", []string{"203.0.113.9"}, true, "192.0.2.10"},
		{"garbage hop keeps RemoteAddr", "127.0.0.1:5000", []string{"not-an-ip"}, true, "127.0.0.1"},
		{"no XFF keeps RemoteAddr", "127.0.0.1:5000", nil, true, "127.0.0.1"},
		{"IPv6 zone dropped", "[fe80::1%en0]:5000", nil, false, "fe80::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/admin/projects", nil)
			r.RemoteAddr = c.remote
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, c.trust); got != c.want {
				t.Fatalf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPasswordRules(t *testing.T) {
	if CheckPassword("short") == nil || CheckPassword(string(make([]byte, 73))) == nil {
		t.Fatal("a password outside 15..72 bytes was accepted")
	}
	pw, err := TempPassword()
	if err != nil || CheckPassword(pw) != nil || len(pw) != 24 {
		t.Fatalf("temporary password %d chars, err %v", len(pw), err)
	}
	h, err := HashPassword(pw, 4)
	if err != nil || !PasswordMatches(h, pw) || PasswordMatches(h, pw+"x") {
		t.Fatalf("bcrypt round trip failed: %v", err)
	}
	if ParseRole("super_admin") != RoleSuperAdmin || ParseRole("root") != RoleNone || RoleAdmin.String() != "admin" {
		t.Fatal("role mapping")
	}
}
