// Package replica is the leader/replica pair (S4, D-012): one Node per instance that is either the
// leader (serves PathReplica: register_replica → chunked snapshot → live stream + heartbeats, and
// probes its peer's /health to fence itself) or a replica (follows the leader, applies the stream,
// and promotes itself with epoch+1 when no frame arrives for REDLITE_HEARTBEAT_TIMEOUT). Node
// implements ws.Node (IsLeader, Epoch, ObserveEpoch) and sheet.Observer (the replica hub).
package replica
