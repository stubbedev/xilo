# justfile for xilo — self-hosted Nix binary cache.
# Run `just` to see all available recipes.

set shell := ["bash", "-euo", "pipefail", "-c"]

# Default — list recipes.
default:
    @just --list --unsorted

# ─────────────────────────── Build & Test ───────────────────────────

# Version baked into the binary at link time.
GO_LDFLAGS := "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

# Regenerate templ views (internal/server/views/*_templ.go).
generate:
    templ generate

# Build the binary into ./bin/ (regenerates views first).
build: generate
    mkdir -p bin
    go build -ldflags="{{GO_LDFLAGS}}" -o bin/xilo ./cmd/xilo
    @echo "Built ./bin/xilo"

# Live-reload dev server (air): rebuilds on .go/.templ/.css change.
# Copy xilo.example.yaml to xilo.yaml first.
dev:
    air

# Install into $GOBIN (or $GOPATH/bin).
install:
    go install -ldflags="{{GO_LDFLAGS}}" ./cmd/xilo

# Format (gofmt).
fmt:
    gofmt -w .

# Vet + build + test — the local gate.
lint:
    gofmt -l .
    go vet ./...

test:
    go test ./...

# ─────────────────────────── Codegen ───────────────────────────

# Regenerate the published JSON schema from config.Config. Same dev
# contract as treeman: anything that *can* be regenerated *is*. CI runs
# the read-only `schema-check` variant as the strict gate.
sync-schema: build
    mkdir -p schemas
    ./bin/xilo schema dump --out schemas/xilo.schema.json
    @if [ -n "$(git status --porcelain schemas/xilo.schema.json)" ]; then \
        echo "sync-schema: regenerated schemas/xilo.schema.json"; \
    else \
        echo "sync-schema: schema already in sync"; \
    fi

# Strict read-only check that committed templ output is in sync (CI gate).
templ-check:
    #!/usr/bin/env bash
    set -euo pipefail
    templ generate
    if [ -n "$(git status --porcelain '*_templ.go')" ]; then
        echo "::error::templ output is stale. Run 'just generate' and commit."
        git --no-pager diff -- '*_templ.go'
        exit 1
    fi
    echo "templ output in sync"

# Strict read-only schema check (what CI runs on PRs).
schema-check: build
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p schemas
    ./bin/xilo schema dump --out schemas/xilo.schema.json
    if [ -n "$(git status --porcelain schemas/xilo.schema.json)" ]; then
        echo "::error::JSON schema is stale. Run 'just sync-schema' and commit."
        git --no-pager diff schemas/xilo.schema.json
        exit 1
    fi
    echo "schema in sync"

# Everything CI checks.
check: lint test templ-check schema-check

# ─────────────────────────── Run & Dev ───────────────────────────

# Run the server against ./xilo.yaml (copy xilo.example.yaml first).
run: build
    ./bin/xilo serve

# Create a cache locally: `just cache-create mycache`.
cache-create name:
    ./bin/xilo cache create {{name}}

# ─────────────────────────── Docker ───────────────────────────

# Build the docker image locally.
docker-build:
    docker build -t xilo:dev .

# Run the image with a local data volume on :8080.
docker-run: docker-build
    docker run --rm -p 8080:8080 -v xilo-data:/data xilo:dev

clean:
    rm -rf bin/
