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

# Vet + build + test — the local gate. Views regenerate first: *_templ.go is
# never committed, only built.
lint: generate
    gofmt -l .
    go vet ./...

test: generate
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

# ─────────────────────────── Nix ───────────────────────────

# Update everything: flake inputs + Go deps, then resync vendorHash.
# The only supported way to bump dependencies — never edit hashes by hand.
update:
    nix flake update
    go get -u ./...
    go mod tidy
    just sync-vendor-hash

# Re-pin flake.nix vendorHash from go.mod/go.sum. Same contract as
# sync-schema: anything that can be regenerated is. Run after any dep
# change (`just update` does it for you).
sync-vendor-hash:
    #!/usr/bin/env bash
    set -euo pipefail
    sed -i 's|vendorHash = "[^"]*";|vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";|' flake.nix
    got=$( (nix build .#default --no-link 2>&1 || true) | sed -n 's/.*got: *//p' | head -1)
    if [ -z "$got" ]; then
        echo "sync-vendor-hash: could not extract vendor hash from nix build output" >&2
        exit 1
    fi
    sed -i "s|vendorHash = \"[^\"]*\";|vendorHash = \"$got\";|" flake.nix
    echo "vendorHash → $got"
    nix build .#default --no-link
    echo "nix build OK"

# Strict read-only check: the nix package builds with the committed
# vendorHash (catches go.mod/flake drift; what CI runs on PRs).
nix-check:
    nix build .#default --no-link
    @echo "nix package in sync"

# Everything CI checks.
check: lint test schema-check nix-check

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

# ─────────────────────────── k6 (tests/k6/) ───────────────────────────

# Operations conformance: every wire + admin operation with correctness
# assertions (auth matrix, TOTP cycle, byte-exact NARs on all encodings).
k6-ops:
    docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/ops.js
    docker compose -f tests/k6/compose.yaml down -v

# CLI end-to-end: every xilo subcommand against a containerized server,
# real nix closure push, `nix copy` as the pull verifier. Needs nix + docker.
e2e:
    ./tests/e2e/cli.sh

# Perf numbers: narinfo QPS, NAR pull, push pipeline. Tracked per release.
k6-perf:
    docker compose -f tests/k6/compose.yaml run --rm k6
    docker compose -f tests/k6/compose.yaml down -v

# Integrity soak: hostile GC vs concurrent dedup pushes; any dropped or
# corrupt NAR fails. DURATION=10m just k6-churn for a longer run.
k6-churn:
    XILO_E2E_CONFIG=server-churn.yaml docker compose -f tests/k6/compose.yaml \
        run --rm k6 run --summary-export=/out/summary.json /scripts/churn.js
    docker compose -f tests/k6/compose.yaml down -v

# Churn against a race-detector server build (slow start, catches data races).
k6-race:
    docker compose -f tests/k6/compose.yaml --profile race run --rm k6-race
    docker compose -f tests/k6/compose.yaml --profile race down -v

# Edge-dimension stress: 1000-chunk NAR, 1MiB chunks, 10k-path narinfo storm.
k6-deep:
    docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/deep.js
    docker compose -f tests/k6/compose.yaml down -v

# Pressure: 512-VU storm, 5000rps arrival flood, 128-VU pull wall, client
# aborts mid-NAR, goroutine-leak watch + recovery proof. Scale via env:
# STORM_VUS=1024 FLOOD_RPS=10000 DROP_BUDGET=999999 just k6-pressure
# (raise DROP_BUDGET when pushing FLOOD_RPS past the hardware ceiling —
# drops then mean finite capacity, not collapse; failures stay at zero)
k6-pressure:
    docker compose -f tests/k6/compose.yaml run --rm \
        -e STORM_VUS -e FLOOD_RPS -e PULL_VUS -e DURATION_S -e DROP_BUDGET \
        k6 run /scripts/pressure.js
    docker compose -f tests/k6/compose.yaml down -v

# Chaos: SIGKILL mid-push, restart, prove nothing corrupted. Needs nix + docker.
chaos:
    ./tests/e2e/chaos.sh

# Head-to-head vs attic on this machine (push, pull, RSS/CPU). ~5 min.
bench-attic:
    ./tests/bench/bench.sh

clean:
    rm -rf bin/
