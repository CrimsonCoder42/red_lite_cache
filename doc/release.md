# Release — how consumers get it

The module is `github.com/CrimsonCoder42/red_lite_cache` (confirm: D-001). Consumers import the public packages only:

```go
import (
    "github.com/CrimsonCoder42/red_lite_cache/reader"
    "github.com/CrimsonCoder42/red_lite_cache/protocol"
)
```

## Tagging (Devin runs these; agents never run git writes)

```bash
git tag v0.1.0
git push origin main --tags
```

Semver: a patch is a fix, a minor adds a field/action (additive), a major is a wire break (D-id required).

## Pinning in backendAdvo

```bash
cd advomedix-v2/backendAdvo
go get github.com/CrimsonCoder42/red_lite_cache@v0.1.0
go mod tidy && go mod vendor
```

`backendAdvo` pins a tag, never `main`. A private repo needs `GOPRIVATE=github.com/CrimsonCoder42/*` and
an SSH remote or a token in `~/.netrc`; neither is committed anywhere.

## Local development against an unreleased version

In `backendAdvo/go.mod`, temporarily:

```
replace github.com/CrimsonCoder42/red_lite_cache => /Users/devinanderson/Desktop/red_lite_cache
```

Remove the `replace` before committing.

## Running the server

```bash
cp .env.example .env
go run ./cmd/redlite               # leader on :8081
REDLITE_ROLE=replica REDLITE_LEADER_ADDRESS=ws://localhost:8081/ws REDLITE_PORT=:8082 go run ./cmd/redlite
```
