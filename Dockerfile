# syntax=docker/dockerfile:1
# ============================================================
# Two-stage build for redlite (after backendAdvo/Dockerfile)
# STAGE 1: builder — compiles the static binary (migrations are embedded)
# STAGE 2: runtime — alpine with the binary only
#
# .env is NOT copied into the image: configuration comes from the environment
# (docker-compose.yml, or the deploy workflow). No vendor/, no sibling checkout.
# ============================================================
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/redlite ./cmd/redlite

FROM alpine:3.21
RUN adduser -D -u 10001 redlite
USER redlite
COPY --from=build /out/redlite /usr/local/bin/redlite
ENV REDLITE_PORT=:8091
EXPOSE 8091
HEALTHCHECK --interval=10s --timeout=3s CMD wget -qO- "http://localhost${REDLITE_PORT}/health" || exit 1
ENTRYPOINT ["redlite"]
