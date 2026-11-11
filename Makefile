# red_lite_cache — dev loop (D-018). Target names match backendAdvo/Makefile where they mean the same thing.
# Reads .env when present (never committed); every value can also come from the environment.
-include .env
export

DB_NAME        ?= redlite
DB_URL         ?= postgres://postgres:postgres@127.0.0.1:5434/$(DB_NAME)?sslmode=disable
TEST_DB_URL    ?= postgres://postgres:postgres@127.0.0.1:5434/redlite_test?sslmode=disable
LEADER_PORT    ?= :8095
REPLICA_PORT   ?= :8096
LEADER_ADMIN   ?= :8097
REPLICA_ADMIN  ?= :8098

.PHONY: proto up cluster down wipe logs smoke test-persist postgres createdb dropdb c_m migrateup migratedown \
        sqlc test bench server dev dev-pair dev-leader dev-replica clean db_start db_reset fresh_start build vet

# Create a new migration pair: make c_m name=add_x
c_m:
	migrate create -ext sql -dir db/migration -seq $(name)

# ── docker compose (doc/docker_ready_2026-11-11) ─────────────────────────────────────────────
# postgres + leader + the dev-project bootstrap; waits until the bootstrap wrote .redlite/dev-project.env.
COMPOSE_UID = REDLITE_BOOTSTRAP_UID=$$(id -u) REDLITE_BOOTSTRAP_GID=$$(id -g)

up:
	mkdir -p .redlite
	$(COMPOSE_UID) docker compose up -d --build
	docker compose wait bootstrap

# … plus the replica on 127.0.0.1:8096.
cluster:
	mkdir -p .redlite
	$(COMPOSE_UID) docker compose --profile cluster up -d --build
	docker compose wait bootstrap

down:            # stop; data kept
	docker compose --profile cluster down

wipe:            # stop and delete the data (volume redlite_pg); .redlite/ must go too: its key is dead
	docker compose --profile cluster down -v

logs:
	docker compose --profile cluster logs -f

# The whole stack under its own compose project and ports, then torn down (developer data untouched).
smoke:
	bash testing/compose_smoke.sh

# The harness with the persistence tests on (needs `make up`: redlite_test on 127.0.0.1:5434).
test-persist:
	REDLITE_TEST_DATABASE_URL="$(TEST_DB_URL)" bash testing/run.sh

# Postgres only.
postgres:
	docker compose up -d --wait postgres

clean: wipe

createdb:
	docker compose exec postgres createdb --username=postgres --owner=postgres $(DB_NAME)

dropdb:
	docker compose exec postgres dropdb --username=postgres $(DB_NAME)

# The binary migrates itself at boot when REDLITE_PERSIST_ENABLED=true; these are for manual use.
migrateup:
	migrate -path db/migration -database "$(DB_URL)" -verbose up

migratedown:
ifndef n
	$(error Usage: make migratedown n=<number> — you must specify how many migrations to roll back)
endif
	migrate -path db/migration -database "$(DB_URL)" -verbose down $(n)

sqlc:
	sqlc generate

# The wire contract (D-048): regenerate protocol/redlitev1 from proto/ and record the .proto hash
# that protocol/redlitev1's staleness test checks.
PROTOS = proto/redlite/v1/cache.proto proto/redlite/v1/replica.proto
proto:
	protoc -I proto --go_out=. --go_opt=module=github.com/CrimsonCoder42/red_lite_cache \
		--go-grpc_out=. --go-grpc_opt=module=github.com/CrimsonCoder42/red_lite_cache $(PROTOS)
	cat $(PROTOS) | shasum -a 256 | cut -d' ' -f1 > protocol/redlitev1/PROTO_SHA256

build:
	go build ./...

vet:
	go vet ./...

# Every package under -race, logged to testing/runs/ (the harness, never raw go test).
test:
	bash testing/run.sh

# Store, sweep and broadcast benchmarks; compare runs with benchstat.
bench:
	go test -run='^$$' -bench=. -benchmem -count=6 ./internal/cache/ ./internal/sheet/ | tee testing/runs/bench_$$(date +%Y%m%d_%H%M%S).txt

server:
	go run ./cmd/redlite

# One leader under air on REDLITE_PORT (default :8095).
dev:
	air

# Leader :8095 (admin :8097) + replica :8096 (admin :8098) under air, over gRPC (D-048), each fencing
# the other through REDLITE_PEER_ADDRESS.
# Never :8081/:8082 — a local advo_cache may be running there.
dev-pair:
	$(MAKE) -j2 dev-leader dev-replica

dev-leader:
	REDLITE_ROLE=leader REDLITE_PORT=$(LEADER_PORT) REDLITE_INSTANCE_ID=leader-dev REDLITE_CLUSTER_TOKEN=dev-cluster-token \
	REDLITE_ADMIN_PORT=$(LEADER_ADMIN) REDLITE_PEER_ADDRESS=localhost$(REPLICA_PORT) REDLITE_PEER_ADMIN_ADDRESS=http://localhost$(REPLICA_ADMIN) \
	air -build.cmd "go build -o ./tmp/leader ./cmd/redlite" -build.bin "./tmp/leader"

dev-replica:
	REDLITE_ROLE=replica REDLITE_PORT=$(REPLICA_PORT) REDLITE_INSTANCE_ID=replica-dev REDLITE_CLUSTER_TOKEN=dev-cluster-token \
	REDLITE_ADMIN_PORT=$(REPLICA_ADMIN) REDLITE_LEADER_ADDRESS=localhost$(LEADER_PORT) \
	REDLITE_PEER_ADDRESS=localhost$(LEADER_PORT) REDLITE_PEER_ADMIN_ADDRESS=http://localhost$(LEADER_ADMIN) \
	air -build.cmd "go build -o ./tmp/replica ./cmd/redlite" -build.bin "./tmp/replica"

# POSTGRES_DB creates redlite and docker/postgres/ creates redlite_test on a fresh volume; the
# binary migrates itself at boot (db/store.go Migrate).
db_start: postgres

db_reset: dropdb createdb migrateup

fresh_start: wipe up
