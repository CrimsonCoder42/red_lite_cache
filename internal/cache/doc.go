// Package cache is the per-sheet key/value store: a plain map behind a sync.RWMutex, lazy TTL expiry
// on read, one process-wide janitor, jittered TTLs, and the __NULL__ placeholder. Arrives with S1.
package cache
