# Migrations

`golang-migrate` SQL pairs: `NNNNNN_name.up.sql` + `NNNNNN_name.down.sql`. Applied by `cmd/redlite`
at boot when `REDLITE_PERSIST_ENABLED=true`. First pair arrives with S5 (`doc/persist/design.md`).

Agents never edit a shipped migration; a change is a new pair. The hook and the delete guard protect this folder.
