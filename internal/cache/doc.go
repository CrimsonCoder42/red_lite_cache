// Package cache is the per-sheet key/value store: a plain map behind a sync.RWMutex (D-002), lazy
// TTL expiry on read plus an Expire pass the sheet janitor drives (D-003), a jittered TTL policy, and
// Groups, the ordered index (forward and reversed B+trees, D-044) through which DeletePrefix and
// DeleteSuffix touch only the keys that match (R7). The reader's local map uses the same Groups.
// The __NULL__ placeholder is protocol.NullValue; the store treats it as any other value.
package cache
