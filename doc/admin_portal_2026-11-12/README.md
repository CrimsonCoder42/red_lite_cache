# admin_portal_2026-11-12

The red_lite_cache admin portal (Devin, 2026-11-12): research and plan, then the backend wave (slices S1–S5,
built and verified). The portal app (S6–S8) is the next wave.

| File | What |
|---|---|
| `PLAN.md` | The plan: data model, hashing, bootstrap + lock-down, auth, roles, projects/keys, monitoring, protection, the `portal/` app, slices S1–S8 with tests and estimates, decisions D1–D10 |
| `000003_admin.proposed.sql`, `000003_admin.down.proposed.sql` | The migration as proposed; Devin applied it as `db/migration/000003_admin.{up,down}.sql` |
| `PROGRESS.md` | One line per step of the backend wave |
| `REPORT.md` | Harness lines, pristine check, checker verdicts and fixes |
| `WALKTHROUGH.md` | Curl walkthrough against the dev stack + lock-down + break-glass |
| `out/` | Raw harness output (gitignored) |

References read (read-only): `advomedix-v2/advomedix_admin` (layout, ui, admin_list, user_admin, auth pages,
apiClient, tokenManager, authSlice, roles, theme) and `advomedix-v2/backendAdvo` (admin bootstrap, routes,
auth/access/rate_limit middleware, password, token, schema). No `.env` file was opened in either repo or here.

D1–D8 decided by Devin (D-051…D-055); D9/D10 belong to the portal wave.
