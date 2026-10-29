#!/bin/bash
# advomedix-readonly.sh — PreToolUse hook for Bash|Edit|Write|MultiEdit|NotebookEdit.
#
# advomedix-v2 is READ-ONLY from this project. Nothing a Claude session in red_lite_cache runs may change it.
#   File tools (Edit/Write/MultiEdit/NotebookEdit): blocked if the target resolves inside advomedix-v2.
#   Bash: any command that mentions advomedix / advo_production / advo_cache / backendAdvo is allowed ONLY if every
#         segment is a read-only command (cat, grep, ls, head, tail, wc, find without -delete/-exec, sed -n, git log/show/
#         diff/blame/ls-files/status/grep/rev-parse, …) and nothing is redirected to a file.
# Exit 0 = allow, exit 2 = block (Claude Code shows stderr to the model). If the check itself fails, it blocks.
INPUT="$(cat)"
command -v python3 >/dev/null 2>&1 || { echo "advomedix-readonly: python3 missing, blocked" >&2; exit 2; }
printf '%s' "$INPUT" | python3 -c '
import sys, json, os, re, shlex
ROOT = os.path.realpath("/Users/devinanderson/Desktop/advo_production")
MENTION = re.compile(r"advomedix|advo_production|advo_cache|backendAdvo", re.I)
READ_CMDS = {"cat","head","tail","less","grep","egrep","fgrep","rg","ls","wc","file","stat","du","diff","cmp",
             "sort","uniq","cut","tr","jq","echo","printf","pwd","cd","true","test","[","basename","dirname",
             "realpath","readlink","tree","md5","shasum","column","nl","fold","strings","sed","find","git","go"}
GIT_READ = {"log","show","diff","blame","ls-files","ls-tree","status","grep","rev-parse","cat-file","describe",
            "shortlog","branch","remote","reflog","tag"}
GO_READ = {"doc","list","version","env"}
FIND_BAD = {"-delete","-exec","-execdir","-ok","-okdir","-fprint","-fprint0","-fprintf","-fls"}
def block(msg):
    sys.stderr.write("advomedix-readonly BLOCKED: " + msg + "\nadvomedix-v2 is read-only from red_lite_cache. Do not retry another way; report it under LEFT FOR DEVIN.\n")
    sys.exit(2)
def inside(p, cwd):
    if not p: return False
    full = os.path.realpath(os.path.join(cwd, os.path.expanduser(p)))
    return full == ROOT or full.startswith(ROOT + os.sep)
try:
    d = json.loads(sys.stdin.read())
except Exception:
    block("hook input unreadable")
tool = d.get("tool_name","")
ti = d.get("tool_input") or {}
cwd = d.get("cwd") or os.getcwd()
if tool in ("Edit","Write","MultiEdit","NotebookEdit"):
    p = ti.get("file_path") or ti.get("notebook_path") or ""
    if inside(p, cwd) or MENTION.search(os.path.realpath(os.path.join(cwd, os.path.expanduser(p))) if p else ""):
        if inside(p, cwd): block("%s on %s" % (tool, p))
    sys.exit(0)
if tool != "Bash":
    sys.exit(0)
cmd = ti.get("command") or ""
if not MENTION.search(cmd):
    sys.exit(0)
if re.search(r"\$\(|`|<\(|>\(", cmd):
    block("command substitution in a command that touches advomedix")
# redirects: only to /dev/null or fd dup
for m in re.finditer(r"(\d*)(>>?|&>)\s*(\S+)", cmd):
    tgt = m.group(3)
    if tgt not in ("/dev/null",) and not tgt.startswith("&"):
        block("redirect to %s" % tgt)
try:
    lex = shlex.shlex(cmd.replace("\n"," ; "), posix=True, punctuation_chars=True)
    lex.whitespace_split = True; lex.commenters = ""
    toks = list(lex)
except ValueError:
    block("unparseable command")
segs, cur, i = [], [], 0
while i < len(toks):
    t = toks[i]
    if t and all(c in "();|&<>" for c in t):
        if "<" in t or ">" in t:            # redirect: targets were vetted above; drop it, its target, a leading fd
            if cur and cur[-1].isdigit(): cur.pop()
            i += 2; continue
        segs.append(cur); cur = []
    else:
        cur.append(t)
    i += 1
segs.append(cur)
for seg in segs:
    while seg and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", seg[0]): seg = seg[1:]
    if not seg: continue
    w = os.path.basename(seg[0])
    if w in ("2","1","/dev/null"): continue
    if w not in READ_CMDS:
        block("\"%s\" is not a read-only command" % w)
    args = seg[1:]
    if w == "sed" and (any(a.startswith("-i") or a == "--in-place" for a in args) or "-n" not in args):
        block("sed must be sed -n and never -i")
    if w == "find" and FIND_BAD & set(args):
        block("find with %s" % ", ".join(sorted(FIND_BAD & set(args))))
    if w == "git":
        rest = [a for a in args if not a.startswith("-")]
        i = 0
        while i < len(args):
            if args[i] in ("-C","-c","--git-dir","--work-tree"): i += 2; continue
            if args[i].startswith("-"): i += 1; continue
            break
        sub = args[i] if i < len(args) else ""
        if sub not in GIT_READ:
            block("git %s" % (sub or "(no subcommand)"))
        if sub in ("branch","tag","remote") and any(a in ("-d","-D","-m","-M","add","remove","rm","set-url","rename","-f","--delete") for a in args[i+1:]):
            block("git %s with a write flag" % sub)
    if w == "go":
        if not args or args[0] not in GO_READ:
            block("go %s" % (args[0] if args else ""))
sys.exit(0)
'
exit $?
