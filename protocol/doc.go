// Package protocol defines every frame exchanged between a red_lite_cache server, its replicas and
// the embeddable reader. It is the only place a wire type lives; internal/ws, internal/replica and
// reader import it and define nothing of their own. The v1 field list in doc/protocol/wire.md is the
// compatibility contract: additive changes only.
package protocol

// Version is the protocol version this module speaks. A breaking wire change bumps it (D-id required).
const Version = 1
