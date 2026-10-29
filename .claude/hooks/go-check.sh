#!/usr/bin/env bash
# PostToolUse hook for Edit|Write.
# Formats a touched .go file and vets the module. Exit 0 = clean or nothing to do. Exit 2 = gofmt or go vet failed.
set -uo pipefail

ROOT="${CLAUDE_PROJECT_DIR:-/Users/devinanderson/Desktop/red_lite_cache}"

file=$(jq -r '.tool_input.file_path // empty')
[ -n "$file" ] || exit 0
case "$file" in *.go) ;; *) exit 0 ;; esac
case "$file" in "$ROOT"/vendor/*) exit 0 ;; "$ROOT"/*) ;; *) exit 0 ;; esac
[ -f "$file" ] || exit 0

cd "$ROOT" || exit 0

if ! out=$(gofmt -l -w "$file" 2>&1); then
  printf 'gofmt failed on %s\n%s\n' "$file" "$out" >&2
  exit 2
fi

if ! out=$(go vet ./... 2>&1); then
  printf 'go vet failed\n%s\n' "$out" >&2
  exit 2
fi

# ─────────────────────────────────────────────────────────────────────────────
# PROJECT INVARIANTS — cheap greps for the mistakes advo_cache made and this
# module must not repeat. Each one is a pattern, not a style opinion.
# ─────────────────────────────────────────────────────────────────────────────
rel="${file#"$ROOT"/}"
viol=""
add() { viol="$viol\n  $1"; }

# 1. No second logger. Package code logs through internal/logger only.
case "$rel" in
  internal/logger/*|cmd/redlite/main.go|*_test.go) ;;
  *)
    grep -nE '\b(fmt\.Print(ln|f)?|log\.(Print|Fatal|Panic)(ln|f)?)\(' "$file" >/dev/null 2>&1 \
      && add "fmt.Print*/log.Print* — use logger.L (CLAUDE.md ## Logging)"
    ;;
esac

# 2. Config is read once. Nothing outside internal/config touches the environment.
case "$rel" in
  internal/config/*|*_test.go) ;;
  *)
    grep -nE '\bos\.(Getenv|LookupEnv)\(' "$file" >/dev/null 2>&1 \
      && add "os.Getenv outside internal/config — add it to config.Config"
    ;;
esac

# 3. No goroutine-per-entry expiry (advo_cache cache.go:44-51 did this).
grep -nE 'go func\(\) *\{[^}]*time\.Sleep' "$file" >/dev/null 2>&1 \
  && add "goroutine sleeping to expire an entry — expiry lives in the value + one janitor (go-caching.md)"

# 4. A consumer-visible type never lives under internal/.
case "$rel" in
  internal/*)
    grep -nE '^// (Reader|Client) ' "$file" >/dev/null 2>&1 \
      && add "consumer-facing type under internal/ — move it to reader/ or protocol/"
    ;;
esac

# 5. Wire messages are typed structs with json tags, not map[string]any.
case "$rel" in
  protocol/*|internal/ws/*|reader/*)
    grep -nE 'map\[string\](any|interface\{\})\{' "$file" | grep -ivE 'value|payload|test' >/dev/null 2>&1 \
      && add "untyped map used as a message — define it in protocol/"
    ;;
esac

if [ -n "$viol" ]; then
  printf 'project invariant violation in %s%b\n' "$rel" "$viol" >&2
  exit 2
fi
exit 0
