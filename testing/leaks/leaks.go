// Covers: memory and goroutine bounds

// Package leaks is the D-038 leak suite: every place a key, frame, session, lock, queue entry or
// goroutine can accumulate is filled, observed above its baseline, drained by the real event
// (expiry, delete, close, kick, revoke), and asserted back at baseline. Every test names its five
// steps — BASELINE, FILL, OBSERVE, DRAIN, ASSERT — through Steps, so a run log reads as the proof.
//
// Shared helpers only; the tests are one file per layer (store_test.go, conn_test.go,
// reader_test.go) plus e2e_test.go. Harness: testing/integration (StartLeader, WithClock,
// GoroutineBaseline, HeapAlloc).
package leaks

import (
	"testing"
	"time"
)

// Steps logs the five D-038 steps of one test, each with the wall time since the test began.
type Steps struct {
	t     testing.TB
	start time.Time
}

// NewSteps starts the step log of t.
func NewSteps(t testing.TB) *Steps { return &Steps{t: t, start: time.Now()} }

func (s *Steps) log(step, format string, args ...any) {
	s.t.Helper()
	s.t.Logf("%-8s +%-8s "+format, append([]any{step, time.Since(s.start).Round(time.Millisecond)}, args...)...)
}

// Baseline logs the BASELINE step.
func (s *Steps) Baseline(format string, args ...any) {
	s.t.Helper()
	s.log("BASELINE", format, args...)
}

// Fill logs the FILL step.
func (s *Steps) Fill(format string, args ...any) { s.t.Helper(); s.log("FILL", format, args...) }

// Observe logs the OBSERVE step.
func (s *Steps) Observe(format string, args ...any) { s.t.Helper(); s.log("OBSERVE", format, args...) }

// Drain logs the DRAIN step.
func (s *Steps) Drain(format string, args ...any) { s.t.Helper(); s.log("DRAIN", format, args...) }

// Assert logs the ASSERT step.
func (s *Steps) Assert(format string, args ...any) { s.t.Helper(); s.log("ASSERT", format, args...) }

// Elapsed is the wall time since NewSteps (PERFORMANCE: L1, L2, L12 report it; > 10 s is a finding).
func (s *Steps) Elapsed() time.Duration { return time.Since(s.start) }

// Above fails the test unless peak > base: a test that cannot show its peak above baseline did not
// exercise the path (D-038).
func Above(t testing.TB, what string, peak, base int) {
	t.Helper()
	if peak <= base {
		t.Fatalf("OBSERVE %s: peak %d is not above baseline %d — the test did not exercise the path", what, peak, base)
	}
}

// AtMost fails the test unless got <= base+tol. Goroutine counts use it: a count below the
// baseline means goroutines of an earlier test (already alive when the baseline was sampled) have
// since exited — not a leak; only a count above baseline + tol is one.
func AtMost(t testing.TB, what string, got, base, tol int) {
	t.Helper()
	if got > base+tol {
		t.Errorf("ASSERT %s: got %d, want at most %d + %d", what, got, base, tol)
	}
}

// Within fails the test unless |got-want| <= tol.
func Within(t testing.TB, what string, got, want, tol int) {
	t.Helper()
	if d := got - want; d > tol || d < -tol {
		t.Errorf("ASSERT %s: got %d, want %d ± %d", what, got, want, tol)
	}
}
