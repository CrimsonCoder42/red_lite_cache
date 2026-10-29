// Package sheet is the tenant registry. A Sheet is a cache.Store, the collaborator connections that
// receive its replicate/invalidate broadcasts, a sequence, and the R6 sweep log; one mutex per sheet
// orders apply + broadcast (D-014). Sheets are created by autocreate on open or the admin API
// (D-013), restored from persistence or the leader with their bcrypt hash (K12), and never deleted
// when the last collaborator leaves (K13). RunJanitor is the one expiry goroutine (D-003).
package sheet
