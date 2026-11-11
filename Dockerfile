# syntax=docker/dockerfile:1
# ============================================================
# redlite images (after backendAdvo/Dockerfile)
# STAGE build:     compiles the static binary (migrations are embedded)
# STAGE bootstrap: the one-shot dev-project bootstrap for docker-compose (curl + jq + one script);
#                  never deployed, built only with `target: bootstrap`
# STAGE runtime:   alpine with the binary only — the default target
#
# .env is NOT in the build context (.dockerignore): configuration comes from the environment
# (docker-compose.yml, or the deploy workflow). No secrets in any layer.
# ============================================================
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/redlite ./cmd/redlite

FROM alpine:3.21 AS bootstrap
RUN apk add --no-cache curl jq && adduser -D -u 10001 redlite
COPY docker/bootstrap/bootstrap.sh /usr/local/bin/redlite-bootstrap
USER redlite
ENTRYPOINT ["redlite-bootstrap"]

FROM alpine:3.21 AS runtime
RUN adduser -D -u 10001 redlite
USER redlite
COPY --from=build /out/redlite /usr/local/bin/redlite
# 8095: the gRPC client port (Cache, Replica, grpc.health.v1); 8097: admin REST + GET /health (D-048)
ENV REDLITE_PORT=:8095 REDLITE_ADMIN_PORT=:8097
EXPOSE 8095 8097
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=12 \
  CMD wget -qO- "http://127.0.0.1${REDLITE_ADMIN_PORT}/health" >/dev/null || exit 1
ENTRYPOINT ["redlite"]
