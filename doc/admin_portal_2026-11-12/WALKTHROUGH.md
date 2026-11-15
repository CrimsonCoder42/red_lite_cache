# Admin portal backend — curl walkthrough (dev stack)

For Devin, against `make up` (leader admin port 127.0.0.1:8097). Not run by the agent against your stack.
Every body goes through a file (`--data @file`), never the command line; the admin key is read from
your shell environment (`.env` is loaded by your own shell or the Makefile — the agent never reads it).
Secrets end up only in `/tmp/rl/` (mode 700); delete the folder at the end.

```bash
umask 077; mkdir -p /tmp/rl && cd /tmp/rl
C=http://127.0.0.1:8097
# the admin key, as a curl config file so it never appears in `ps` (bootstrap.sh does the same)
printf 'header = "Authorization: Bearer %s"\n' "$REDLITE_ADMIN_KEY" > key.cfg

# 1. bootstrap the first super_admin (once; password 15..72 bytes)
cat > boot.json <<'EOF'
{"email":"devin@example.test","name":"Devin","password":"choose-a-long-passphrase-here"}
EOF
curl -sS -K key.cfg -X POST --data @boot.json $C/admin/bootstrap; echo            # 201 {"actor":"user:…","role":"super_admin",…}

# 2. sign in (cookie jar; the cookie is HttpOnly, SameSite=Strict, Secure off in development)
cat > login.json <<'EOF'
{"email":"devin@example.test","password":"choose-a-long-passphrase-here"}
EOF
curl -sS -c jar -X POST --data @login.json $C/admin/auth/login; echo              # 200 {"role":"super_admin","expires_at":…}

# 3. who am I
curl -sS -b jar $C/admin/auth/me; echo                                              # 200

# 4. create a project: the API key is in this reply ONCE — copy it into the consumer's .env now
cat > proj.json <<'EOF'
{"name":"walkthrough"}
EOF
curl -sS -b jar -X POST --data @proj.json $C/admin/projects > proj.out; echo        # 201
# (contains "api_key":"rlk_…" — keep proj.out private, delete after copying)

# 5. create a viewer: the temporary password is in this reply ONCE
cat > user.json <<'EOF'
{"email":"viewer@example.test","name":"Viewer","role":"viewer"}
EOF
curl -sS -b jar -X POST --data @user.json $C/admin/users > user.out; echo           # 201
TMP=$(sed -E 's/.*"temporary_password":"([^"]+)".*/\1/' user.out)

# 6. the viewer signs in, must change the password, then is refused a write
printf '{"email":"viewer@example.test","password":"%s"}' "$TMP" > vlogin.json
curl -sS -c vjar -X POST --data @vlogin.json $C/admin/auth/login; echo              # 200 … "must_change_password":true
curl -sS -b vjar -o /dev/null -w '%{http_code}\n' $C/admin/projects                 # 403 password change required
printf '{"current_password":"%s","new_password":"viewer-long-passphrase-1"}' "$TMP" > vpw.json
curl -sS -b vjar -o /dev/null -w '%{http_code}\n' -X POST --data @vpw.json $C/admin/auth/password   # 204
curl -sS -b vjar -o /dev/null -w '%{http_code}\n' $C/admin/projects                 # 200 (viewer may read)
curl -sS -b vjar -o /dev/null -w '%{http_code}\n' -X POST --data @proj.json $C/admin/projects       # 403 forbidden

# 7. monitoring
curl -sS -b jar "$C/admin/stats"; echo                                              # nodes: role, epoch, started_at, counts
curl -sS -b jar "$C/admin/audit?limit=20"; echo                                     # newest first: admin_bootstrap, admin_login, …

# 8. sign out; the old cookie is dead
curl -sS -b jar -c jar -o /dev/null -w '%{http_code}\n' -X POST $C/admin/auth/logout # 204
curl -sS -b jar -o /dev/null -w '%{http_code}\n' $C/admin/auth/me                   # 401

# 9. bootstrap is closed for good
curl -sS -K key.cfg -o /dev/null -w '%{http_code}\n' -X POST --data @boot.json $C/admin/bootstrap  # 410

cd / && rm -r /tmp/rl
```

## Lock-down after the first sign-in (PLAN.md §3)

1. Production: `REDLITE_ADMIN_PORT=127.0.0.1:8097` (or the Tailscale IP), firewall 8097/8098; TLS in front
   (`tailscale serve` or Caddy) with `REDLITE_ADMIN_TRUSTED_PROXY=true`.
2. Bootstrap once (step 1), sign in (step 2), confirm step 9 answers 410.
3. Rotate `REDLITE_ADMIN_KEY` on both nodes (doc/release.md "Rotating the admin key"): the old value was typed
   into a shell. The key stays for bootstrap.sh, the peer node and break-glass.
4. Optionally create a second super_admin (step 5 with `"role":"super_admin"`).
5. `GET /admin/audit` shows `admin_bootstrap` and `admin_login`.

Break-glass: the admin key acts as super_admin — `POST /admin/users/{id}/reset-password` with `-K key.cfg`
returns a temporary password for a locked-out or forgotten account.
