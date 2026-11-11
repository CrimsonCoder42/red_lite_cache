#!/bin/sh
# redlite dev bootstrap (doc/docker_ready_2026-11-11, D-047): one-shot, run by docker-compose after the
# leader is healthy. Through the existing admin REST API only, it makes sure one development project
# with one sheet of its own exists, and writes how to connect to $OUT_DIR/dev-project.env (mode 600).
#
#   Idempotent: a finished file for the same project and sheet → the key is checked, nothing created.
#   Resumable:  every secret is written to a .partial file before the call that needs it, so a run that
#               stops part-way is finished by the next one. A project whose key was lost (a timeout on
#               the create, a deleted .partial) gets a NEW key from the admin key — never a second project.
#   Quiet:      the API key, the admin key and the sheet password never appear in the logs or in any
#               command's arguments (curl reads headers and bodies from files).
#
# Exit 0 = ready; non-zero = not ready, with one line saying why.
set -eu
umask 077

URL="${REDLITE_BOOTSTRAP_URL:-http://leader:8097}" # the admin port (D-048)
ADMIN="${REDLITE_ADMIN_KEY:?REDLITE_ADMIN_KEY is required}"
PROJECT="${REDLITE_DEV_PROJECT:-dev}"
SHEET="${REDLITE_DEV_SHEET:-}"
[ -n "$SHEET" ] || SHEET="$PROJECT" # sheet names are unique per node: default to the project's name
ADDR="${REDLITE_DEV_ADDR:-127.0.0.1:8095}" # the gRPC client address
OUT_DIR="${REDLITE_BOOTSTRAP_OUT:-/out}"
HOST_PATH="${REDLITE_BOOTSTRAP_HOST_PATH:-./.redlite/dev-project.env}"
FILE="$OUT_DIR/dev-project.env"
PART="$OUT_DIR/.dev-project.env.partial"

fail() { echo "bootstrap: FAILED: $*" >&2; exit 1; }
note() { echo "bootstrap: $*"; }

valid_name() { printf '%s' "$1" | grep -Eq '^[A-Za-z0-9._-]{1,128}$'; }
valid_name "$PROJECT" || fail "REDLITE_DEV_PROJECT must be 1-128 of [A-Za-z0-9._-]"
valid_name "$SHEET" || fail "REDLITE_DEV_SHEET must be 1-128 of [A-Za-z0-9._-]"
[ -d "$OUT_DIR" ] && [ -w "$OUT_DIR" ] ||
	fail "$OUT_DIR is not writable (the host directory ${HOST_PATH%/*}: create it, and on Linux run with REDLITE_BOOTSTRAP_UID=\$(id -u) — 'make up' does both)"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# api METHOD PATH TOKEN [BODY] → CODE, BODY_OUT. Token and body go through files (curl -K / --data @),
# never argv, so neither shows in ps; the response is read from a file.
api() {
	printf 'header = "Authorization: Bearer %s"\n' "$3" >"$WORK/cfg"
	if [ $# -ge 4 ]; then
		printf '%s' "$4" >"$WORK/body"
		set -- "$1" "$2" --data "@$WORK/body" -H 'Content-Type: application/json'
	else
		set -- "$1" "$2"
	fi
	m="$1" p="$2"
	shift 2
	CODE=$(curl -sS -m 30 -K "$WORK/cfg" -o "$WORK/resp" -w '%{http_code}' -X "$m" "$@" "$URL$p") ||
		fail "$m $p: leader unreachable at $URL"
	BODY_OUT=$(cat "$WORK/resp")
}

errmsg() { printf '%s' "$BODY_OUT" | jq -r '.error? // empty' 2>/dev/null || true; }

# load FILE: KEY=VALUE lines, read as data — never sourced, so no value is ever executed.
load() {
	while IFS='=' read -r k v; do
		case "$k" in
		REDLITE_DEV_PROJECT) F_PROJECT=$v ;;
		REDLITE_DEV_PROJECT_ID) REDLITE_DEV_PROJECT_ID=$v ;;
		REDLITE_DEV_API_KEY) REDLITE_DEV_API_KEY=$v ;;
		REDLITE_DEV_SHEET) F_SHEET=$v ;;
		REDLITE_DEV_SHEET_PASSWORD) REDLITE_DEV_SHEET_PASSWORD=$v ;;
		esac
	done <"$1"
}

write_file() { # $1 = target
	{
		echo "# red_lite_cache development project — written by the compose bootstrap. DEV ONLY; keep private."
		echo "REDLITE_DEV_PROJECT=$PROJECT"
		echo "REDLITE_DEV_PROJECT_ID=${REDLITE_DEV_PROJECT_ID:-}"
		echo "REDLITE_DEV_API_KEY=${REDLITE_DEV_API_KEY:-}"
		echo "REDLITE_DEV_SHEET=$SHEET"
		echo "REDLITE_DEV_SHEET_PASSWORD=$REDLITE_DEV_SHEET_PASSWORD"
		echo "REDLITE_DEV_ADDR=$ADDR"
	} >"$1.tmp" || fail "cannot write ${HOST_PATH%/*}/${1##*/}"
	chmod 600 "$1.tmp" && mv -f "$1.tmp" "$1" || fail "cannot write ${HOST_PATH%/*}/${1##*/}"
}

# project_id NAME → the id of the project called NAME (admin listing), or empty.
project_id() {
	api GET "/admin/projects?limit=200" "$ADMIN"
	[ "$CODE" = 200 ] || fail "list projects: HTTP $CODE $(errmsg)"
	printf '%s' "$BODY_OUT" | jq -r --arg n "$1" '.items[] | select(.name == $n) | .project_id' | head -n 1
}

# ensure_sheet: create the sheet with the project's key. 201 = created; 409 = the name is taken, which
# is only fine if the sheet is this project's own (names are unique per node): checked with an
# export by the project's key, which answers 404 for another project's sheet.
ensure_sheet() {
	api POST "/admin/projects/$REDLITE_DEV_PROJECT_ID/sheets" "$REDLITE_DEV_API_KEY" \
		"$(jq -nc --arg n "$SHEET" --arg p "$REDLITE_DEV_SHEET_PASSWORD" '{name:$n, password:$p}')"
	case "$CODE" in
	201) note "sheet '$SHEET' created" ;;
	409)
		api GET "/admin/sheets/$SHEET/export" "$REDLITE_DEV_API_KEY"
		[ "$CODE" = 200 ] || fail "sheet name '$SHEET' is taken by another project: set REDLITE_DEV_SHEET to another name"
		note "sheet '$SHEET' already exists in project '$PROJECT'"
		;;
	401) fail "the API key for project '$PROJECT' was refused (revoked, or the database was wiped while $HOST_PATH was kept): remove $HOST_PATH and run again" ;;
	*) fail "create sheet '$SHEET': HTTP $CODE $(errmsg)" ;;
	esac
}

# 1. Already bootstrapped: same project and sheet → check the key, create nothing.
if [ -f "$FILE" ]; then
	F_PROJECT="" F_SHEET="" REDLITE_DEV_PROJECT_ID="" REDLITE_DEV_API_KEY="" REDLITE_DEV_SHEET_PASSWORD=""
	load "$FILE"
	[ "$F_PROJECT" = "$PROJECT" ] && [ "$F_SHEET" = "$SHEET" ] ||
		fail "$HOST_PATH is for project '$F_PROJECT' sheet '$F_SHEET', not '$PROJECT'/'$SHEET': use another REDLITE_BOOTSTRAP_DIR, or remove the file"
	[ -n "$REDLITE_DEV_API_KEY" ] && [ -n "$REDLITE_DEV_PROJECT_ID" ] && [ -n "$REDLITE_DEV_SHEET_PASSWORD" ] ||
		fail "$HOST_PATH is incomplete: remove it and run again"
	ensure_sheet
	chmod 600 "$FILE"
	note "ready: project '$PROJECT' ($REDLITE_DEV_PROJECT_ID), sheet '$SHEET' — connection details in $HOST_PATH"
	exit 0
fi

# 2. Resume a run that stopped part-way, or start: the sheet password is on disk before any call.
REDLITE_DEV_PROJECT_ID="" REDLITE_DEV_API_KEY="" REDLITE_DEV_SHEET_PASSWORD=""
if [ -f "$PART" ]; then
	F_PROJECT="" F_SHEET=""
	load "$PART"
	[ "$F_PROJECT" = "$PROJECT" ] && [ "$F_SHEET" = "$SHEET" ] || { REDLITE_DEV_PROJECT_ID="" REDLITE_DEV_API_KEY="" REDLITE_DEV_SHEET_PASSWORD=""; }
	[ -n "$REDLITE_DEV_SHEET_PASSWORD" ] && note "resuming a bootstrap that stopped part-way"
fi
if [ -z "$REDLITE_DEV_SHEET_PASSWORD" ]; then
	REDLITE_DEV_SHEET_PASSWORD=$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')
	write_file "$PART"
fi

# 3. The project. Its API key is returned once: written to .partial before anything else happens.
if [ -z "$REDLITE_DEV_API_KEY" ]; then
	api POST /admin/projects "$ADMIN" "$(jq -nc --arg n "$PROJECT" '{name:$n, label:"compose-bootstrap"}')"
	case "$CODE" in
	201)
		REDLITE_DEV_PROJECT_ID=$(printf '%s' "$BODY_OUT" | jq -r '.project_id // empty' 2>/dev/null || true)
		REDLITE_DEV_API_KEY=$(printf '%s' "$BODY_OUT" | jq -r '.api_key // empty' 2>/dev/null || true)
		[ -n "$REDLITE_DEV_PROJECT_ID" ] && [ -n "$REDLITE_DEV_API_KEY" ] || fail "create project '$PROJECT': unexpected reply (no project_id/api_key)"
		write_file "$PART"
		note "project '$PROJECT' created ($REDLITE_DEV_PROJECT_ID)"
		;;
	409)
		# The project exists but its key never reached this disk (a timed-out create, a deleted
		# .partial): issue it a new key with the admin key instead of creating a second project.
		REDLITE_DEV_PROJECT_ID=$(project_id "$PROJECT")
		[ -n "$REDLITE_DEV_PROJECT_ID" ] || fail "project '$PROJECT' exists but is not listed"
		api POST "/admin/projects/$REDLITE_DEV_PROJECT_ID/keys" "$ADMIN" '{"label":"compose-bootstrap-recovered"}'
		[ "$CODE" = 201 ] || fail "issue a new key for project '$PROJECT': HTTP $CODE $(errmsg)"
		REDLITE_DEV_API_KEY=$(printf '%s' "$BODY_OUT" | jq -r '.api_key // empty' 2>/dev/null || true)
		[ -n "$REDLITE_DEV_API_KEY" ] || fail "issue a new key for project '$PROJECT': unexpected reply"
		write_file "$PART"
		note "project '$PROJECT' already existed ($REDLITE_DEV_PROJECT_ID): issued it a new key"
		;;
	401) fail "the admin key was refused: REDLITE_ADMIN_KEY must match the leader's" ;;
	*) fail "create project '$PROJECT': HTTP $CODE $(errmsg)" ;;
	esac
fi

# 4. The sheet, then the finished file.
ensure_sheet
write_file "$FILE"
rm -f "$PART"
note "ready: project '$PROJECT' ($REDLITE_DEV_PROJECT_ID), sheet '$SHEET' — connection details in $HOST_PATH (mode 600)"
