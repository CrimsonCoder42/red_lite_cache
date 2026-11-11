#!/bin/bash
# compose_smoke.sh — the whole `docker compose up` stack, checked from the outside, then torn down
# (doc/docker_ready_2026-11-11). Runs only when called (`make smoke`); never part of testing/run.sh.
#
# It uses its OWN compose project name, host ports and key directory, so a developer's running stack
# and its data (project red_lite_cache, volume redlite_pg) are never touched:
#   REDLITE_SMOKE_PROJECT    default redlite_smoke
#   REDLITE_SMOKE_PORTS      leader/replica/postgres/leader-admin/replica-admin host ports,
#                            default "18095 18096 15433 18097 18098" (gRPC client ports, then the admin ports, D-048)
# Steps: up → healthy → /health says leader → the bootstrap file exists (mode 600) and its key is
# accepted → a second bootstrap creates nothing → a reader signs in with the key, sets, reads, clears →
# `docker compose restart leader` → the reader reconnects by itself and the key is back from PostgreSQL
# → down -v (this project's volume only). Exit code non-zero on any failure.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

P="${REDLITE_SMOKE_PROJECT:-redlite_smoke}"
read -r LP RP PP LA RA <<<"${REDLITE_SMOKE_PORTS:-18095 18096 15433 18097 18098}"
LA="${LA:-18097}" RA="${RA:-18098}"
STAMP="$(date +%Y%m%d_%H%M%S)"
DIR="$ROOT/testing/runs/compose_smoke_$STAMP" # gitignored; holds this run's key file
mkdir -p "$DIR"
LOG="$ROOT/testing/runs/${STAMP}__compose_smoke.log"

export REDLITE_HOST_LEADER_PORT="$LP" REDLITE_HOST_REPLICA_PORT="$RP" REDLITE_HOST_PG_PORT="$PP"
export REDLITE_HOST_LEADER_ADMIN_PORT="$LA" REDLITE_HOST_REPLICA_ADMIN_PORT="$RA"
export REDLITE_BOOTSTRAP_DIR="$DIR"
export REDLITE_BOOTSTRAP_UID="$(id -u)" REDLITE_BOOTSTRAP_GID="$(id -g)"
export REDLITE_IMAGE_TAG="smoke" # its own image tags: the developer's redlite:dev is never rebuilt
export REDLITE_ADMIN_KEY="smoke-admin-$STAMP-$RANDOM"
export REDLITE_CLUSTER_TOKEN="smoke-cluster-$STAMP-$RANDOM"
export REDLITE_PG_PASSWORD="smoke-pg-$STAMP-$RANDOM"
# Its own (empty) env file: compose never reads the developer's .env for this project, so every
# value is a default or one of the throwaway values exported above.
: >"$DIR/smoke.env"
DC=(docker compose -p "$P" --env-file "$DIR/smoke.env")

say() { echo "smoke: $*" | tee -a "$LOG"; }
die() { say "FAIL: $*"; exit 1; }

for port in "$LP" "$RP" "$PP" "$LA" "$RA"; do
	# netstat, not lsof: lsof without sudo misses listeners owned by other users (a native postgres)
	if netstat -an 2>/dev/null | grep LISTEN | grep -qE "[.:]$port "; then
		die "host port $port is in use; set REDLITE_SMOKE_PORTS"
	fi
done

cleanup() {
	code=$?
	say "down -v (project $P only)"
	"${DC[@]}" --profile cluster down -v --remove-orphans >>"$LOG" 2>&1 || true
	if [ "$code" -eq 0 ]; then say "PASS (log $LOG)"; else say "FAILED with exit $code (log $LOG)"; fi
	exit "$code"
}
trap cleanup EXIT

say "up (project $P, ports $LP/$RP/$PP, admin $LA/$RA)"
t0=$(date +%s)
"${DC[@]}" up -d --build >>"$LOG" 2>&1 || die "docker compose up"
"${DC[@]}" wait bootstrap >>"$LOG" 2>&1 || { "${DC[@]}" logs bootstrap >>"$LOG" 2>&1; die "bootstrap exited non-zero (see the log)"; }
say "stack up and bootstrapped in $(($(date +%s) - t0)) s"

health=""
for _ in $(seq 1 60); do
	health="$(curl -fsS -m 2 -H "Authorization: Bearer $REDLITE_CLUSTER_TOKEN" "http://127.0.0.1:$LA/health" 2>/dev/null || true)"
	[ -n "$health" ] && break
	sleep 1
done
echo "$health" | grep -q '"status":"ok"' || die "/health not ok: $health"
echo "$health" | grep -q '"role":"leader"' || die "/health does not report leader: $health"
say "/health ok, role leader"

FILE="$DIR/dev-project.env"
[ -f "$FILE" ] || die "bootstrap file $FILE missing"
mode="$(stat -c %a "$FILE" 2>/dev/null)" || mode="$(stat -f %Lp "$FILE")" # GNU/busybox, else BSD
[ "$mode" = "600" ] || die "bootstrap file mode $mode, want 600"
val() { grep -E "^$1=" "$FILE" | head -n 1 | cut -d= -f2-; } # read as data, never sourced
REDLITE_DEV_API_KEY="$(val REDLITE_DEV_API_KEY)" REDLITE_DEV_PROJECT_ID="$(val REDLITE_DEV_PROJECT_ID)"
REDLITE_DEV_SHEET="$(val REDLITE_DEV_SHEET)" REDLITE_DEV_PROJECT="$(val REDLITE_DEV_PROJECT)"
code="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $REDLITE_DEV_API_KEY" \
	-H 'Content-Type: application/json' -d "{\"name\":\"$REDLITE_DEV_SHEET\",\"password\":\"x\"}" \
	"http://127.0.0.1:$LA/admin/projects/$REDLITE_DEV_PROJECT_ID/sheets")"
[ "$code" = "409" ] || die "the bootstrap key was not accepted (create of the existing sheet answered $code, want 409)"
say "key file present (mode 600), key accepted"
logs="$("${DC[@]}" logs 2>/dev/null || true)" # captured first: grep -q on a pipe could hide a match (SIGPIPE)
for secret in "$REDLITE_DEV_API_KEY" "$(val REDLITE_DEV_SHEET_PASSWORD)" "$REDLITE_ADMIN_KEY"; do
	if grep -qF -- "$secret" <<<"$logs"; then die "a secret appears in the stack's logs"; fi
done
say "no key, password or admin key in the logs"

"${DC[@]}" run --rm bootstrap >>"$LOG" 2>&1 || die "second bootstrap run failed"
n="$(curl -fsS -m 5 -H "Authorization: Bearer $REDLITE_ADMIN_KEY" "http://127.0.0.1:$LA/admin/projects?limit=200" |
	grep -o "\"name\":\"$REDLITE_DEV_PROJECT\"" | wc -l | tr -d ' ')"
[ "$n" = "1" ] || die "after a second bootstrap there are $n projects named $REDLITE_DEV_PROJECT, want 1"
say "second bootstrap: idempotent (1 project)"

# A failed bootstrap is visible (non-zero, one reason line) and creates nothing.
if "${DC[@]}" run --rm -e REDLITE_DEV_PROJECT=smoke-other bootstrap >>"$LOG" 2>&1; then
	die "a bootstrap for another project over an existing key file exited 0"
fi
if "${DC[@]}" run --rm -e REDLITE_BOOTSTRAP_OUT=/tmp -e REDLITE_ADMIN_KEY=wrong -e REDLITE_DEV_PROJECT=smoke-badkey bootstrap >>"$LOG" 2>&1; then
	die "a bootstrap with a wrong admin key exited 0"
fi
grep -q "FAILED: the admin key was refused" "$LOG" || die "the wrong-admin-key failure gave no reason line"
n="$(curl -fsS -m 5 -H "Authorization: Bearer $REDLITE_ADMIN_KEY" "http://127.0.0.1:$LA/admin/projects?limit=200" |
	{ grep -oE '"name":"smoke-(other|badkey)"' || true; } | wc -l | tr -d ' ')" # no match is the pass case
[ "$n" = "0" ] || die "a failed bootstrap left $n project(s) behind"
say "failed bootstraps: non-zero exit with a reason, nothing created"

# A lost key file (fresh output dir, project already there): recovered with a new key, no 2nd project.
"${DC[@]}" run --rm -e REDLITE_BOOTSTRAP_OUT=/tmp bootstrap >>"$LOG" 2>&1 || die "bootstrap could not recover a project whose key file was lost"
grep -q "already existed .*issued it a new key" "$LOG" || die "recovery did not issue a new key"
n="$(curl -fsS -m 5 -H "Authorization: Bearer $REDLITE_ADMIN_KEY" "http://127.0.0.1:$LA/admin/projects?limit=200" |
	{ grep -o "\"name\":\"$REDLITE_DEV_PROJECT\"" || true; } | wc -l | tr -d ' ')"
[ "$n" = "1" ] || die "recovery left $n projects named $REDLITE_DEV_PROJECT, want 1"
say "lost key file: recovered with a new key, still 1 project"

say "reader round trip + leader restart (testing/compose, gated test)"
REDLITE_COMPOSE_TEST=1 REDLITE_COMPOSE_KEYFILE="$FILE" REDLITE_COMPOSE_RESTART=1 REDLITE_COMPOSE_PROJECT="$P" REDLITE_COMPOSE_ENV_FILE="$DIR/smoke.env" \
	bash testing/run.sh --run TestComposeReaderRoundTrip testing/compose >>"$LOG" 2>&1 || die "reader round trip (see the log)"
grep -q -- "--- PASS: TestComposeReaderRoundTrip" "$LOG" || die "reader round trip did not pass"
say "reader signed in with the key, set/get/clear ok, reconnected after the leader restart, key restored from PostgreSQL"
