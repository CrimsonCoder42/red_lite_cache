// Package protocol defines every frame exchanged between a red_lite_cache server, its replicas and
// the embeddable reader. It is the only place a wire type lives; internal/ws, internal/replica and
// reader import it and define nothing of their own. The v1 field list in doc/protocol/wire.md is the
// compatibility contract: additive changes only.
//
// Discriminators: a client → server frame carries Action; a server → client broadcast carries Type;
// a direct reply to a request carries neither and has Success. A reader tells a reply from a
// broadcast by the presence of "type".
package protocol

// Version is the protocol version this module speaks. Every change since v1 is additive (new
// omitempty fields, new actions and types), so it is still 1. A breaking wire change bumps it
// (D-id required). Sent in the open reply as protocol_version (D-010).
const Version = 1
