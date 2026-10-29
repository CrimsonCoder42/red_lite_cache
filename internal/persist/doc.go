// Package persist is the PostgreSQL write-behind layer: a bounded op channel, one writer goroutine
// that batches into transactions, boot-time hydration, and a TTL cleaner. The memory copy is the
// source of truth; this is the durable copy. Rules: .claude/rules/backend/go-persist.md. Arrives with S5.
package persist
