// Package cache is the per-sheet key/value store: a plain map behind a sync.RWMutex (D-002), lazy
// TTL expiry on read plus an Expire pass the sheet janitor drives (D-003), a jittered TTL policy, and
// a first/last-segment index plus a `<root>:patient:<uuid>` head index (D-037) so DeletePrefix and
// DeleteSuffix touch only the keys that can match (R7).
// The __NULL__ placeholder is protocol.NullValue; the store treats it as any other value.
package cache
