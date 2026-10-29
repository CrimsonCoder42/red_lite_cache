// Package integration holds the black-box tests: each file boots a real leader (and a replica when
// the test needs one) on a free port with httptest and drives it through reader/ or raw frames.
//
// Line 1 of every test file is `// Covers: <action or package>` — the counterpart of advomedix's
// `# Handler:` header. `grep -rn "// Covers:"` finds the covering test.
//
// Helpers that arrive with S2: StartLeader(t) *Server, StartReplica(t, leaderAddr) *Server,
// RawConn(t, addr) for frame-level probes. Persist tests read REDLITE_TEST_DATABASE_URL and
// t.Skip("SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)") without it.
package integration
