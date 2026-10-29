# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/redlite ./cmd/redlite

FROM alpine:3.21
RUN adduser -D -u 10001 redlite
USER redlite
COPY --from=build /out/redlite /usr/local/bin/redlite
EXPOSE 8081
HEALTHCHECK --interval=10s --timeout=3s CMD wget -qO- http://localhost:8081/health || exit 1
ENTRYPOINT ["redlite"]
