# Progress — docker compose up → a ready cache

- Baseline: 388 | 373 | 0 | 15 (testing/runs/latest.json, 20261111_104418). Pristine copy: scratchpad/docker_pristine. Ports 8095/8096/5433 free; no redlite volume existed.
- [x] 1 .dockerignore
- [x] 2 Dockerfile
- [x] 3 docker-compose.yml + docker/postgres + docker/bootstrap
- [x] 4 Makefile docker targets
- [x] 5 testing/compose_smoke.sh
- [x] 6 gated Go test testing/compose
- [x] PHASE 2 — harness (persist on), smoke ×2 + clean clone, checker (criterion 4 failed → fixed, smoke re-run green), docs
