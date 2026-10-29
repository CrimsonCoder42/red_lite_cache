#!/bin/bash
# PROBE — temporary. Delete or promote at CLEANUP. Topic: ___  Date: ___  Question: ___
#
# Raw-frame probe: see exactly what the server sends. Needs `websocat` and a running server:
#   REDLITE_PORT=:8099 go run ./cmd/redlite     (in another terminal; stop it when done)
# Assert nothing you don't yet know. Print frames, read them, then decide.

ADDR="${REDLITE_PROBE_ADDR:-ws://localhost:8099/ws}"
SHEET="${REDLITE_PROBE_SHEET:-probe-sheet}"
PASS="${REDLITE_PROBE_PASSWORD:-probe-secret}"

{
  echo "{\"action\":\"open\",\"sheet\":\"$SHEET\",\"password\":\"$PASS\"}"
  sleep 0.2
  echo '{"action":"set","key":"probe:1","value":{"n":1}}'
  sleep 0.2
  echo '{"action":"get","key":"probe:1"}'
  sleep 0.2
} | websocat -t "$ADDR" | sed 's/^/  <- /'
