// Package replica is the follower side: connects to the leader, applies full_state and replicate_*
// frames, watches the heartbeat and promotes itself on timeout. Arrives with S4.
package replica
