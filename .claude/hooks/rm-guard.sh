#!/bin/bash
# rm-guard.sh — PreToolUse hook for the Bash tool.
# Blocks (exit 2) any delete that is not on the approved list below. Allows everything else (exit 0).
# Approved deletes:
#   FREE      testing/runs, bin, coverage.out (recursive allowed); .DS_Store and *.swp anywhere
#   PROBES    testing/_probe/<file> (not TEMPLATE.sh / TEMPLATE_test.go)
#   TRACKED   a single NON-SOURCE file that git tracks and that has no uncommitted edits
#             (recoverable with: git show HEAD:<path>). Source files (SOURCE_EXT) always go to Devin.
# Never, whatever the class: see NEVER_* below, and anything outside the repo.
# A delete must be a plain `rm`/`rmdir`/`unlink` command: no variables, no cd + relative path,
# no find -delete, no xargs, no shell -c, no script one-liners.
# If this script cannot run its check, it blocks any command that mentions a delete.

INPUT="$(cat)"
DEL_RE='(^|[^[:alnum:]_.-])(rm|rmdir|unlink|shred|srm|trash|rimraf)([^[:alnum:]_.-]|$)|-delete|rmtree|unlinkSync|rmSync'

if ! command -v python3 >/dev/null 2>&1; then
  if printf '%s' "$INPUT" | grep -Eq "$DEL_RE"; then
    echo "rm-guard: python3 not found, so deletes cannot be checked. Blocked. List the file for Devin." >&2
    exit 2
  fi
  exit 0
fi

PY="$(cat <<'PYEOF'
import sys, os, re, json, shlex, glob, fnmatch, subprocess

DEL = re.compile(r"(?<![\w.-])(rm|rmdir|unlink|shred|srm|trash|rimraf)(?![\w.-])"
                 r"|-delete\b|rmtree|os\.remove|os\.unlink|unlinkSync|rmSync|git\s+clean")
RM_WORDS = {"rm", "rmdir", "unlink"}
SAFE_FIRST = {"grep", "egrep", "fgrep", "rg", "echo", "printf", "cat", "head", "tail", "ls", "wc",
              "git", "jq", "sort", "uniq", "cut", "tr", "diff", "date", "true", "test", "[", "touch", "mkdir"}
SHELLS = {"sh", "bash", "zsh", "dash", "ksh", "fish", "eval", "source", "."}
CD_WORDS = {"cd", "pushd", "popd"}

NEVER_COMPONENTS = {".git", ".claude", "vendor", ".github", "secrets", "db"}
NEVER_PREFIXES = ("db/migration", "deploy")
NEVER_FILES = {"doc/DECISIONS.md", "doc/CHANGELOG.md", "doc/ASSUMPTIONS.md", "doc/scope/SCOPE.md",
               "doc/protocol/wire.md", "doc/README.md"}
NEVER_BASENAMES = [".env", ".env.*", "*.env", "go.mod", "go.sum", "Makefile", "Dockerfile",
                   "docker-compose*.yml", ".gitignore", "CLAUDE.md", "run.sh", "TEMPLATE.sh", "TEMPLATE.md",
                   "TEMPLATE_test.go", "README.md", "LICENSE"]

FREE_DIR = re.compile(r"^testing/runs(/|$)|^bin(/|$)")
JUNK = [".DS_Store", "*.swp", "coverage.out"]
SOURCE_EXT = {".go", ".sql", ".sh", ".py", ".ts", ".js"}
BASELINE = re.compile(r"^$")
PROBE = re.compile(r"^testing/_probe/(?!TEMPLATE)[^/]+$")


def block(msg):
    sys.stderr.write("rm-guard BLOCKED: " + msg + "\nThis is final. Do not try another way to delete it. "
                     "Put the path in your report under LEFT FOR DEVIN with the reason.\n")
    sys.exit(2)


def git(repo, *args):
    return subprocess.run(["git", "-C", repo] + list(args), stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def check_target(repo, cwd, raw, recursive, word):
    if re.search(r"[$`{}]", raw) or raw.startswith("~"):
        block("'%s' uses a variable, ~ or braces. Write the literal path." % raw)
    if ".." in raw.split("/"):
        block("'%s' contains '..'. Write the path without it." % raw)
    full = os.path.realpath(os.path.join(cwd, raw))
    rel = os.path.relpath(full, repo)
    if rel == "." or rel.startswith(".."):
        block("'%s' is the repo root or outside the repo." % raw)
    rel = rel.replace(os.sep, "/")
    parts = rel.split("/")
    base = parts[-1]
    if NEVER_COMPONENTS & set(parts):
        block("'%s' is inside a protected folder." % rel)
    if any(rel == p or rel.startswith(p + "/") for p in NEVER_PREFIXES):
        block("'%s' is inside a protected folder." % rel)
    if rel in NEVER_FILES or any(fnmatch.fnmatch(base, pat) for pat in NEVER_BASENAMES):
        block("'%s' is a protected file." % rel)
    if "/" not in rel and base.lower().endswith(".pdf"):
        block("'%s' is a protected file." % rel)
    if FREE_DIR.search(rel):
        return
    if word == "rmdir":
        return  # rmdir only removes empty directories
    if os.path.isdir(full) and not os.path.islink(full):
        block("'%s' is a directory outside the test-output folders. Delete files one at a time." % rel)
    if recursive:
        block("recursive delete is only allowed inside testing/runs and bin ('%s')." % rel)
    if any(fnmatch.fnmatch(base, pat) for pat in JUNK) or BASELINE.search(rel) or PROBE.search(rel):
        return
    if os.path.splitext(base)[1].lower() in SOURCE_EXT:
        block("'%s' is a source file. Source deletes are Devin's; list it under LEFT FOR DEVIN." % rel)
    tracked = git(repo, "ls-files", "--error-unmatch", "--", rel).returncode == 0
    if not tracked:
        block("'%s' is not tracked by git, so it cannot be recovered." % rel)
    if git(repo, "diff", "--quiet", "HEAD", "--", rel).returncode != 0:
        block("'%s' has uncommitted edits; git show HEAD: would lose them." % rel)


def main(data):
    cmd = (data.get("tool_input") or {}).get("command") or ""
    if not DEL.search(cmd):
        sys.exit(0)
    cwd = data.get("cwd") or os.getcwd()
    repo = os.environ.get("CLAUDE_PROJECT_DIR") or ""
    if not repo:
        r = subprocess.run(["git", "-C", cwd, "rev-parse", "--show-toplevel"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        repo = r.stdout.decode().strip()
    if not repo:
        block("cannot find the repo root.")
    repo = os.path.realpath(repo)

    lex = shlex.shlex(cmd.replace("\n", " ; "), posix=True, punctuation_chars=True)
    lex.whitespace_split = True
    lex.commenters = ""
    try:
        tokens = list(lex)
    except ValueError:
        block("the command could not be parsed. Run the delete as its own plain rm command.")

    segments, cur, i = [], [], 0
    while i < len(tokens):
        t = tokens[i]
        if t and all(c in "();|&<>" for c in t):
            if "<" in t or ">" in t:          # a redirect: drop it, its target, and a leading fd number
                if cur and cur[-1].isdigit():
                    cur.pop()
                i += 2
                continue
            segments.append(cur)
            cur = []
        else:
            cur.append(t)
        i += 1
    segments.append(cur)

    cleaned = []
    for seg in segments:
        while seg and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", seg[0]):
            seg = seg[1:]
        if seg:
            cleaned.append(seg)
    has_cd = any(os.path.basename(s[0]) in CD_WORDS for s in cleaned)

    for seg in cleaned:
        word = os.path.basename(seg[0])
        if word in RM_WORDS:
            flags, targets, ended = [], [], False
            for a in seg[1:]:
                if not ended and a == "--":
                    ended = True
                elif not ended and a.startswith("-") and len(a) > 1:
                    flags.append(a)
                else:
                    targets.append(a)
            recursive = any(f in ("--recursive", "--dir") or (not f.startswith("--") and re.search(r"[rRd]", f)) for f in flags)
            if not targets:
                block("rm with no path.")
            for raw in targets:
                if not os.path.isabs(raw) and has_cd:
                    block("'%s' is a relative path in a command that also uses cd. Use the absolute path." % raw)
                if re.search(r"[*?\[]", raw):
                    if re.search(r"[$`{}]", raw) or raw.startswith("~") or ".." in raw.split("/"):
                        check_target(repo, cwd, raw, recursive, word)
                    hits = glob.glob(os.path.join(cwd, raw))
                    if not hits:
                        block("'%s' matches no file." % raw)
                    for h in hits:
                        check_target(repo, cwd, h, recursive, word)
                else:
                    check_target(repo, cwd, raw, recursive, word)
        elif word in SHELLS and (len(seg) == 1 or seg[1].startswith("-")):
            block("a delete may not be run through '%s'. Run a plain rm command." % " ".join(seg[:2]))
        elif word in SAFE_FIRST or word in CD_WORDS:
            continue
        elif DEL.search(" ".join(seg)):
            block("a delete may only be a plain rm command (found one inside '%s ...')." % word)
    sys.exit(0)


try:
    raw_in = sys.stdin.read()
    payload = json.loads(raw_in)
except Exception:
    if DEL.search(raw_in if "raw_in" in dir() else ""):
        block("the hook input could not be read.")
    sys.exit(0)
try:
    main(payload)
except SystemExit:
    raise
except Exception as e:
    if DEL.search(json.dumps(payload)):
        block("internal guard error (%s)." % e)
    sys.exit(0)
PYEOF
)"

printf '%s' "$INPUT" | python3 -c "$PY"
exit $?