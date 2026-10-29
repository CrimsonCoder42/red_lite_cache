// Package reader is the embeddable client. A consumer imports this package, calls New with a Config,
// and reads from a local map that the leader keeps in sync over a WebSocket. Reads never touch the
// network. Arrives with slice S3 (doc/scope/SCOPE.md).
package reader
