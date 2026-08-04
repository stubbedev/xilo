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

# Rebuild the Tailwind stylesheet (internal/server/static/xilo-tw.css) from the
# templ views + templui component sources. The output is a generated artifact
# (git-ignored, embedded via //go:embed); every build recipe runs this first.
css:
    TAILWINDCSS='nix run nixpkgs#tailwindcss_4 --' ./scripts/build-css.sh
    @echo "Built internal/server/static/xilo-tw.css"

# Build the binary into ./bin/ (regenerates views first).
build: css generate
    mkdir -p bin
    go build -ldflags="{{GO_LDFLAGS}}" -o bin/xilo ./cmd/xilo
    @echo "Built ./bin/xilo"

# Live-reload dev server (air): rebuilds on .go/.templ/.css change.
# Copy xilo.example.yaml to xilo.yaml first.
dev:
    #!/usr/bin/env sh
    # Open the admin UI once the server is accepting; air runs in the
    # foreground, so its hot-reload restarts never re-trigger this.
    url="http://localhost$(awk -F'"' '/^listen:/{print $2}' xilo.yaml)"
    (until curl -sf -o /dev/null "$url"; do sleep 0.3; done; xdg-open "$url") &
    air

# Install into $GOBIN (or $GOPATH/bin).
install:
    go install -ldflags="{{GO_LDFLAGS}}" ./cmd/xilo

# Format (gofmt).
fmt:
    gofmt -w .

# Vet + build + test — the local gate. Views regenerate first: *_templ.go is
# never committed, only built.
lint: css generate
    gofmt -l .
    go vet ./...
    # The client-only build (flake packages.xilo-cli, no internal/server) must
    # keep compiling; CI runs the same build.
    go build -tags noserver -o /dev/null ./cmd/xilo

# -race matches CI exactly — a push must never learn about a race from CI.
test: css generate
    go test -race ./...

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

# Rebuild the server image the k6 rig runs, from the working tree, and drop any
# state from a previous run. `docker compose run` happily reuses a stale image,
# so without this a k6 suite can pass (or fail) against a binary that is not
# the code you just changed. Every conformance recipe below depends on it.
k6-image:
    docker compose -f tests/k6/compose.yaml down -v
    docker compose -f tests/k6/compose.yaml build xilo

# Operations conformance: every wire + admin operation with correctness
# assertions (auth matrix, TOTP cycle, byte-exact NARs on all encodings).
k6-ops: k6-image
    docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/ops.js
    docker compose -f tests/k6/compose.yaml down -v

# Multi-tenant conformance: registration + approval, plans, the three quotas
# (caches/storage/members), plan-gated org creation, cross-account isolation,
# and the registration rate limit. Runs against the multi_tenant config.
k6-mt: k6-image
    XILO_E2E_CONFIG=server-mt.yaml docker compose -f tests/k6/compose.yaml \
        run --rm k6 run /scripts/mt.js
    docker compose -f tests/k6/compose.yaml down -v

# The exact conformance trio CI's `ops` job runs, in the same order. Run this
# before pushing anything that changes the wire contract (a status code, a
# request or response field): `just check` does NOT cover the k6 suites, and CI
# is where you'd otherwise find out.
k6-conformance: k6-ops
    docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/deep.js
    docker compose -f tests/k6/compose.yaml down -v
    XILO_E2E_CONFIG=server-mt.yaml docker compose -f tests/k6/compose.yaml \
        run --rm k6 run /scripts/mt.js
    docker compose -f tests/k6/compose.yaml down -v

# CLI end-to-end: every xilo subcommand against a containerized server,
# real nix closure push, `nix copy` as the pull verifier. Needs nix + docker.
e2e:
    ./tests/e2e/cli.sh

# Perf numbers: narinfo QPS, NAR pull, push pipeline. Tracked per release.
k6-perf: k6-image
    docker compose -f tests/k6/compose.yaml run --rm k6
    docker compose -f tests/k6/compose.yaml down -v

# Perf with load spread across 4 accounts on a multi_tenant server: same
# scenarios, per-account scoped push tokens, tenant chosen per VU.
k6-perf-mt: k6-image
    XILO_E2E_CONFIG=server-mt.yaml docker compose -f tests/k6/compose.yaml \
        run --rm -e TENANTS=4 k6
    docker compose -f tests/k6/compose.yaml down -v

# Integrity soak: hostile GC vs concurrent dedup pushes; any dropped or
# corrupt NAR fails. DURATION=10m just k6-churn for a longer run.
k6-churn: k6-image
    XILO_E2E_CONFIG=server-churn.yaml docker compose -f tests/k6/compose.yaml \
        run --rm k6 run --summary-export=/out/summary.json /scripts/churn.js
    docker compose -f tests/k6/compose.yaml down -v

# Integrity soak across 4 accounts on a multi_tenant server with hostile GC.
k6-churn-mt: k6-image
    XILO_E2E_CONFIG=server-mt-churn.yaml docker compose -f tests/k6/compose.yaml \
        run --rm -e TENANTS=4 k6 run --summary-export=/out/summary.json /scripts/churn.js
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

# Pressure with load spread across 4 accounts on a multi_tenant server.
k6-pressure-mt:
    XILO_E2E_CONFIG=server-mt.yaml docker compose -f tests/k6/compose.yaml run --rm \
        -e STORM_VUS -e FLOOD_RPS -e PULL_VUS -e DURATION_S -e DROP_BUDGET -e TENANTS=4 \
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

# ─────────────────────────── Release ───────────────────────────

# Pre-release gate: on the default branch, everything CI checks passing, and
# any generated-artifact / lock / vendorHash drift resynced and committed —
# so a release can never ship something `just check` would have caught.
_release-checks:
    #!/usr/bin/env bash
    set -euo pipefail
    BRANCH=$(git rev-parse --abbrev-ref HEAD)
    DEFAULT_BRANCH=$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's|^origin/||' || true)
    DEFAULT_BRANCH=${DEFAULT_BRANCH:-master}
    if [ "$BRANCH" != "$DEFAULT_BRANCH" ]; then
        echo "Error: not on default branch '$DEFAULT_BRANCH' (currently on '$BRANCH')." >&2
        exit 1
    fi
    # check only *verifies* the schema, so regenerate it first: drift becomes a
    # commit instead of a failed release.
    just sync-schema
    just check
    if [ -n "$(git status --porcelain)" ]; then
        echo "Changes detected (formatting / generated artifacts / schema). Committing..."
        git add -A
        git commit -m "chore: sync generated artifacts for release"
    fi
    echo "Updating flake.lock..."
    nix flake update
    if [ -n "$(git status --porcelain flake.lock)" ]; then
        git add flake.lock
        git commit -m "chore: update flake.lock for release"
    fi
    # Re-pins vendorHash against the new lock and re-runs the nix build.
    just sync-vendor-hash
    if [ -n "$(git status --porcelain flake.nix)" ]; then
        git add flake.nix
        git commit -m "chore: update vendorHash for release"
    fi

# Bump the tag and push. That push does the rest (see RELEASING.md): binaries,
# the GitHub release with generated notes, Docker images, floating major tag —
# so this deliberately does NOT run `gh release create`.
_release LEVEL: _release-checks
    #!/usr/bin/env bash
    set -euo pipefail
    # Not `git describe`: the floating major tag (v1) sits on the same commit as
    # the newest release, which makes describe fall back to its long form.
    cur=$(git tag --sort=-v:refname --list 'v[0-9]*.[0-9]*.[0-9]*' | head -1)
    cur=${cur:-v0.0.0}
    IFS=. read -r major minor patch <<<"${cur#v}"
    case "{{ LEVEL }}" in
        major) new="v$((major + 1)).0.0" ;;
        minor) new="v${major}.$((minor + 1)).0" ;;
        patch) new="v${major}.${minor}.$((patch + 1))" ;;
        *) echo "unknown release level: {{ LEVEL }}" >&2; exit 1 ;;
    esac
    echo "Bumping from $cur to $new"
    git tag -a "$new" -m "Release $new"
    git push origin HEAD
    git push origin "$new"
    echo "Pushed $new — watch it with: gh run list --workflow Release"

# Release a new major version (X.y.z -> X+1.0.0).
release-major: (_release "major")

# Release a new minor version (x.Y.z -> x.Y+1.0).
release-minor: (_release "minor")

# Release a new patch version (x.y.Z -> x.y.Z+1).
release-patch: (_release "patch")

# Preview what versions would be created (dry-run).
release-preview:
    #!/usr/bin/env bash
    cur=$(git tag --sort=-v:refname --list 'v[0-9]*.[0-9]*.[0-9]*' | head -1)
    cur=${cur:-v0.0.0}
    IFS=. read -r major minor patch <<<"${cur#v}"
    echo "Current: $cur"
    echo "  major: v$((major + 1)).0.0"
    echo "  minor: v${major}.$((minor + 1)).0"
    echo "  patch: v${major}.${minor}.$((patch + 1))"
