# gRPC build — progress (2026-11-11)

Request: move the wire to gRPC, then remove WebSocket. Spec: doc/specs/s10_grpc.md.

## Phase 1 — build (done, compile-clean, `go vet ./...` clean)

1. Contract — proto/redlite/v1/{cache,replica}.proto; generated code in protocol/redlitev1;
   `make proto`; PROTO_SHA256 + stale_test.go. Replica gained `Probe` (peer fencing over gRPC).
2. Server — internal/rpc: transport-neutral Conn/router/admission/Handler; StreamTransport;
   CacheService; StreamAuth/UnaryAuth interceptors; NewServer (keepalive, enforcement, health).
   Write is abortable (a Send held by flow control no longer holds the RPC open).
   Empty/unknown ClientMsg → "unknown action", stream kept.
3. Reader — reader/link.go: ws:// → WebSocket, host:port → gRPC; chunks assembled to a typed
   SheetState (bulk build, no JSON round trip); terminal codes from the status ErrorDetail.
4. Replica — internal/replica/grpc.go: Follow (chunked snapshot), Probe, follower gRPC link,
   gRPC peer prober; serveReplica shared by both wires.
5. App/config/docker — REDLITE_ADMIN_PORT (:8097), temporary REDLITE_TRANSPORT; App.Run(ctx, ln,
   adminLn); compose health checks + bootstrap on the admin port; Dockerfile EXPOSE 8095 8097;
   Makefile dev targets; .env.example; compose_smoke.sh admin ports.
6. Tests — harness switch REDLITE_TEST_TRANSPORT (RawConn on both wires, ClientURL/ReplicaURL/
   PeerURL, CloseStatus); reader/grpcfake_test.go; internal/rpc/grpc_test.go (bufconn: every
   message, every status, slow reader, 100k keys); testing/blackbox (address + .proto only).
7. internal/ws kept as a thin adapter.

## Phase 2 — verify (done)

A. Baseline (pristine copy, throwaway Postgres in compose project `redlite_grpcbuild`, port 15444, own env file):
   `389 | 386 | 1 | 2` — the failure is the known jsonb `TestPersistLoop` (out/harness_baseline.txt).
B. Every suite on both wires: grpc `412 | 407 | 1 | 4` (2 extra skips: WS-only subtests), ws `412 | 409 | 1 | 2`.
   Found and fixed on the way: abortable `StreamTransport.Write`; server drops end UNAVAILABLE, never OK; reader
   dial bounded by the handshake deadline; reconnect replay windowed (64); peer admin address (D-049);
   gRPC RESOURCE_EXHAUSTED → 1009; RawConn stall/drain semantics.
C. Benchmarks, 6 interleaved runs, medians: out/bench_ws_vs_grpc.txt. gRPC slower per op (~1.45×) and on
   broadcast (~1.9× at 30); fewer bytes on state; stalled peer ended 58 ms vs 5.1 s. Profile: 66 % syscalls
   (per-message HTTP/2 writes), conversion negligible.
D. Removed: internal/ws (6 files, one mv each, to _delete/internal/ws), coder/websocket, REDLITE_TRANSPORT /
   REDLITE_TEST_TRANSPORT, PathWS, PathReplica, ActionDump, ErrMsgUseReplicaPath, ErrMsgDumpMoved; wire.md
   rewritten. Harness `404 | 401 | 1 | 2` (out/harness_after_removal.txt); smoke PASS (out/compose_smoke.log, run
   directly: `make` loads .env); grep: no websocket import, no "/ws" route, `go list -deps` has no websocket.
E. Checker: out/CHECKER.md (7/7 PASS).
F. Docs: s10 spec, architecture, release (v0.4.0), README, consumer migration, D-048–D-050, CHANGELOG, SCOPE.
