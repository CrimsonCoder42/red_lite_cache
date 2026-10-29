#!/bin/bash
# run.sh — test orchestrator for red_lite_cache.
#
# Usage:
#   bash testing/run.sh                       # every package, -race
#   bash testing/run.sh internal/cache        # one package (relative to repo root)
#   bash testing/run.sh reader integration    # several
#   bash testing/run.sh --list                # packages that have tests
#   bash testing/run.sh --run TestName pkg    # pass -run through
#
# "integration" is shorthand for testing/integration.
#
# OUTPUT
#   testing/runs/<timestamp>__<slug>.log   full transcript
#   testing/runs/latest.log                copy of the most recent
#   testing/runs/latest.json               {total, passed, failed, skipped, packages}
#
# Exit code is the number of failed tests (0 = all passed).
# Persist tests skip without REDLITE_TEST_DATABASE_URL; the summary says so.

set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNS="$ROOT/testing/runs"
mkdir -p "$RUNS"

RUN_FILTER=""
TARGETS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --list) cd "$ROOT" && go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | sed '/^$/d'; exit 0 ;;
    --run) RUN_FILTER="$2"; shift ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "Unknown flag: $1" >&2; exit 2 ;;
    integration) TARGETS+=("./testing/integration/...") ;;
    *) TARGETS+=("./${1#./}/...") ;;
  esac
  shift
done
[ ${#TARGETS[@]} -eq 0 ] && TARGETS=("./...")

STAMP="$(date +%Y%m%d_%H%M%S)"
SLUG="$(printf '%s' "${TARGETS[*]}" | tr -c 'a-zA-Z0-9' '_' | sed 's/_*$//' | cut -c1-40)"
LOG="$RUNS/${STAMP}__${SLUG}.log"

cd "$ROOT" || exit 2
echo "== red_lite_cache tests  $(date -u +%FT%TZ)  targets: ${TARGETS[*]}" | tee "$LOG"
ARGS=(-race -count=1 -v)
[ -n "$RUN_FILTER" ] && ARGS+=(-run "$RUN_FILTER")
go test "${ARGS[@]}" "${TARGETS[@]}" 2>&1 | tee -a "$LOG"
GO_EXIT=${PIPESTATUS[0]}

PASSED=$(grep -cE '^\s*--- PASS' "$LOG" || true)
FAILED=$(grep -cE '^\s*--- FAIL' "$LOG" || true)
SKIPPED=$(grep -cE '^\s*--- SKIP' "$LOG" || true)
TOTAL=$((PASSED + FAILED + SKIPPED))
PKGS=$(grep -E '^(ok|FAIL|\?)\s' "$LOG" | awk '{print $2}' | sort -u | tr '\n' ' ')
PERSIST_SKIP=$(grep -c 'SKIPPED: persist' "$LOG" || true)

{
  echo ""
  echo "== Total | Passed | Failed | Skipped : $TOTAL | $PASSED | $FAILED | $SKIPPED"
  [ "$PERSIST_SKIP" -gt 0 ] && echo "== SKIPPED: persist (no REDLITE_TEST_DATABASE_URL)"
  [ "$GO_EXIT" -ne 0 ] && [ "$FAILED" -eq 0 ] && echo "== go test exited $GO_EXIT with no failing test (build or vet error above)"
} | tee -a "$LOG"

cp "$LOG" "$RUNS/latest.log"
printf '{"timestamp":"%s","targets":"%s","total":%d,"passed":%d,"failed":%d,"skipped":%d,"go_exit":%d,"packages":"%s"}\n' \
  "$STAMP" "${TARGETS[*]}" "$TOTAL" "$PASSED" "$FAILED" "$SKIPPED" "$GO_EXIT" "$PKGS" > "$RUNS/latest.json"

if [ "$FAILED" -eq 0 ] && [ "$GO_EXIT" -ne 0 ]; then exit 1; fi
exit "$FAILED"
