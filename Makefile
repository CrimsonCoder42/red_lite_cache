# red_lite_cache — dev loop (D-018). Target names match backendAdvo/Makefile where they mean the same thing.
# Reads .env when present (never committed); every value can also come from the environment.
-include .env
export

DB_NAME        ?= redlite
DB_CONTAINER   ?= redlite_db
DB_URL         ?= postgres://postgres:postgres@localhost:5433/$(DB_NAME)?sslmode=disable
LEADER_PORT    ?= :8095
REPLICA_PORT   ?= :8096

.PHONY: network postgres createdb dropdb c_m migrateup migratedown sqlc test bench server dev \
        dev-pair dev-leader dev-replica clean db_start db_reset fresh_start build vet

# Create a new migration pair: make c_m name=add_x
c_m:
	migrate create -ext sql -dir db/migration -seq $(name)

network:
	docker network create redlite-network || true

# Postgres only (the leader and replica services are started by `docker compose up`).
postgres: network
	docker compose up -d postgres
	@echo "Waiting for database to be ready..."
	@sleep 5

clean:
	docker compose down
	docker network rm redlite-network || true
	docker volume rm red_lite_cache_redlite_pg || true

createdb:
	docker exec $(DB_CONTAINER) createdb --username=postgres --owner=postgres $(DB_NAME)

dropdb:
	docker exec $(DB_CONTAINER) dropdb --username=postgres $(DB_NAME)

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

# Leader :8095 + replica :8096 under air, each fencing the other through REDLITE_PEER_ADDRESS.
# Never :8081/:8082 — a local advo_cache may be running there.
dev-pair:
	$(MAKE) -j2 dev-leader dev-replica

dev-leader:
	REDLITE_ROLE=leader REDLITE_PORT=$(LEADER_PORT) REDLITE_INSTANCE_ID=leader-dev \
	REDLITE_PEER_ADDRESS=ws://localhost$(REPLICA_PORT) \
	air -build.cmd "go build -o ./tmp/leader ./cmd/redlite" -build.bin "./tmp/leader"

dev-replica:
	REDLITE_ROLE=replica REDLITE_PORT=$(REPLICA_PORT) REDLITE_INSTANCE_ID=replica-dev \
	REDLITE_LEADER_ADDRESS=ws://localhost$(LEADER_PORT)/replica \
	REDLITE_PEER_ADDRESS=ws://localhost$(LEADER_PORT) \
	air -build.cmd "go build -o ./tmp/replica ./cmd/redlite" -build.bin "./tmp/replica"

db_start: network postgres createdb

db_reset: dropdb createdb migrateup

fresh_start: clean network postgres createdb migrateup
