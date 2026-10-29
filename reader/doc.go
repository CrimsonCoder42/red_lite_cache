// Package reader is the embeddable client. A consumer imports this package, calls New with a Config,
// and reads from a local map that the leader keeps in sync over a WebSocket. Reads never touch the
// network.
//
// Guarantees (doc/purpose.md R1–R10, doc/specs/s3_reader.md):
//   - It reconnects forever, alternating LeaderAddr and BackupAddr with capped, jittered backoff (R1).
//   - Delete, DeletePrefix and DeleteSuffix never lose a delete: one issued while disconnected is applied
//     locally, queued, and replayed and acknowledged before the reader reports connected again (R2).
//   - While disconnected, Get, GetRaw, Has and IsNull report a miss (R3).
//   - The local map honours the server's remaining TTL and is bounded by Config.MaxEntries (R4).
//   - A set carries base_seq so the server can refuse a value read before a covering invalidation (R6).
//   - Get decodes straight into the caller's type from immutable bytes: no shared mutable value (R8).
//   - SetWithTTL waits for the server's acknowledgement and returns its failure (R9).
//   - New does not fail because the cache is down; only a refused password fails it (R10).
//
// The package imports only the standard library, protocol/ and github.com/coder/websocket. It has no
// globals, no init and reads no environment; Config is its only input.
package reader
