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

# UI dev server (scripts/dev.sh): a throwaway instance seeded with dummy data
# (caches, tokens, users, orgs, pushed store paths, activity) behind air, so
# every .go/.templ/.css save rebuilds, restarts and patches the open page in
# place (no full reload).
# http://localhost:8090, sign in admin / demo. Ctrl-C stops.
dev:
    ./scripts/dev.sh

# Install into $GOBIN (or $GOPATH/bin).
install:
    go install -ldflags="{{GO_LDFLAGS}}" ./cmd/xilo

# Format. goimports, not gofmt: it is gofmt plus the import block, which the
# golangci fixers do not maintain — a fixer that swaps fmt.Sprintf for
# strconv.Itoa leaves the imports wrong on its own.
fmt:
    goimports -w .

# Vet + build + test — the local gate. Views regenerate first: *_templ.go is
# never committed, only built.
lint: css generate
    # Stdlib-first gate (.golangci.yml). Fixable findings are *applied*, never
    # reported: an error you could have fixed yourself is a slower way to fix
    # it. CI runs the same set read-only, so the fixes have to be committed.
    #
    # Two passes, and the first can't be the gate: when two fixers want the
    # same file golangci applies one and skips the other with a *warning*, so
    # a pass can exit 0 with fixable findings still on disk. The second is the
    # gate — whatever it still reports is genuinely not auto-fixable.
    # goimports after each: the fixers rewrite expressions but not the import
    # block, so this is what makes an applied fix compile.
    golangci-lint run --fix ./... || true
    goimports -w .
    golangci-lint run --fix ./...
    goimports -w .
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
sync-schema:
    mkdir -p schemas
    go run ./tools/schemagen --out schemas/xilo.schema.json
    @if [ -n "$(git status --porcelain schemas/xilo.schema.json)" ]; then \
        echo "sync-schema: regenerated schemas/xilo.schema.json"; \
    else \
        echo "sync-schema: schema already in sync"; \
    fi

# Strict read-only schema check (what CI runs on PRs).
schema-check:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p schemas
    go run ./tools/schemagen --out schemas/xilo.schema.json
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

# Known vulnerabilities in the dependency graph, filtered to the ones actually
# reachable from this code (govulncheck traces call paths, so an advisory in a
# function nothing calls does not fail the build).
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Statement coverage over the code humans maintain, with the generated views
# excluded, against the floor CI enforces. Raising the floor is welcome;
# lowering it needs a reason in the commit message.
coverage:
    #!/usr/bin/env bash
    set -euo pipefail
    floor=82.0
    go test -count=1 -coverprofile=cover.out ./internal/... > /dev/null
    grep -v "_templ.go:" cover.out > cover-filtered.out
    pct=$(go tool cover -func=cover-filtered.out | tail -1 | grep -oE '[0-9.]+%' | tr -d '%')
    rm -f cover.out cover-filtered.out
    echo "coverage ${pct}% (floor ${floor}%)"
    awk -v p="$pct" -v f="$floor" 'BEGIN{exit !(p+0 >= f+0)}' || {
        echo "coverage ${pct}% is below the ${floor}% floor" >&2
        exit 1
    }

# Everything CI checks.
check: lint test schema-check nix-check coverage

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

# Churn against a race-detector server build. The first start compiles the
# whole tree under -race, so the suite waits BOOT_WAIT_S (default 900) for
# /healthz before it begins.
k6-race:
    docker compose -f tests/k6/compose.yaml --profile race run --rm \
        -e DURATION -e BOOT_WAIT_S k6-race
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

# CI compares every Perf run against tests/k6/baselines/*.json
# (tests/k6/compare.sh) and fails on a regression.
#
# The baseline is a RATCHET. A new anchor that is better than the committed one
# is written; one that is worse is refused, because the answer to a slower run
# is a faster server, not a wider band. Override with ALLOW_REGRESSION=1 only
# when the slowdown is understood and accepted, and say so in the commit
# message.
#
# DIR is searched recursively for k6-*-summary.json, so point it at one or
# several unzipped Perf artifacts: the more runs, the better the anchor, since
# the runner's own spread is ~1.7x (see tests/k6/baseline.sh). A dev box is NOT
# a valid source -- this laptop runs the suites 2-4x faster than the 2-core
# runner, so baselining here would hand CI numbers it can never meet.
#
#   gh run download <perf-run-id> -D /tmp/perf
#   just re-baseline /tmp/perf
#
# Re-baseline the perf gate from unzipped CI Perf artifacts.
re-baseline DIR:
    #!/usr/bin/env bash
    set -euo pipefail
    allow=${ALLOW_REGRESSION:-}
    worse=0
    for suite in perf churn pressure; do
        mapfile -t files < <(find "{{ DIR }}" -name "k6-$suite-summary.json" | sort)
        if [ "${#files[@]}" = 0 ]; then
            echo "no k6-$suite-summary.json under {{ DIR }}" >&2
            exit 1
        fi
        out="tests/k6/baselines/$suite.json"
        new=$(mktemp)
        ./tests/k6/baseline.sh "$suite" "${files[@]}" > "$new"
        if [ -f "$out" ]; then
            # Compare anchor by anchor, in each metric's own bad direction.
            while IFS=$'\t' read -r metric stat dir old cur; do
                bad=$(awk -v o="$old" -v c="$cur" -v d="$dir" \
                    'BEGIN{print (d=="lower") ? (c > o) : (c < o)}')
                if [ "$bad" = 1 ]; then
                    printf 'WORSE  %-10s %-42s %-7s %s -> %s\n' "$suite" "$metric" "$stat" "$old" "$cur"
                    worse=$((worse + 1))
                fi
            done < <(jq -r --slurpfile new "$new" '
                .metrics as $old
                | $new[0].metrics | to_entries[]
                | .key as $m | .value.direction as $d
                | .value | to_entries[]
                | select(.key | test("^(p\\(|count$|max$|avg$|med$)"))
                | select($old[$m][.key] != null)
                | [$m, .key, $d, ($old[$m][.key]|tostring), (.value|tostring)] | @tsv' "$out")
        fi
        mv "$new" "$out.pending"
    done
    if [ "$worse" -gt 0 ] && [ -z "$allow" ]; then
        rm -f tests/k6/baselines/*.pending
        echo >&2
        echo "$worse anchor(s) would get worse. Nothing written." >&2
        echo "Make the server faster, or rerun with ALLOW_REGRESSION=1 and explain it in the commit." >&2
        exit 1
    fi
    for f in tests/k6/baselines/*.pending; do mv "$f" "${f%.pending}"; done
    [ "$worse" -gt 0 ] && echo "wrote $worse regressed anchor(s) because ALLOW_REGRESSION is set"
    git --no-pager diff --stat tests/k6/baselines/

# SUITE is perf|churn|pressure; FILE is a k6 --summary-export json.
#
# Apply the CI perf gate to a local summary.
k6-compare SUITE FILE:
    ./tests/k6/compare.sh tests/k6/baselines/{{ SUITE }}.json {{ FILE }} {{ SUITE }}

# TARGET is a Fuzz* function name, DURATION a Go duration (default 60s). CI
# runs every target for 30s; this is for hunting new inputs, which land in
# testdata/fuzz and should be committed when they find something.
#
#   just fuzz FuzzParseHash 5m
#
# Fuzz one target for longer than CI does.
fuzz TARGET DURATION="60s":
    #!/usr/bin/env bash
    set -euo pipefail
    pkg=$(grep -rl "func {{ TARGET }}(" --include='*_test.go' internal | head -1 | xargs dirname)
    if [ -z "$pkg" ]; then echo "no package defines {{ TARGET }}" >&2; exit 1; fi
    echo "fuzzing {{ TARGET }} in ./$pkg for {{ DURATION }}"
    go test "./$pkg" -run '^{{ TARGET }}$' -fuzz '^{{ TARGET }}$' -fuzztime={{ DURATION }} -count=1

# Chaos: SIGKILL mid-push, restart, prove nothing corrupted. Needs nix + docker.
chaos:
    ./tests/e2e/chaos.sh

# Runs every target sequentially, measures push, pull, RSS, CPU and bytes on
# disk, then redraws the README's charts from the result. TARGETS picks a
# subset: `just bench xilo,attic`.
#
# CI runs the same script weekly (.github/workflows/bench.yml) and commits what
# it measured, so the committed numbers describe a 2-core runner rather than
# whichever laptop last ran this.
#
# Head-to-head vs attic, nix-serve-ng and MinIO. ~15 min; docker + nix, idle machine.
bench TARGETS="xilo,attic,nixserve,s3":
    ./tests/bench/bench.sh --targets {{ TARGETS }} --json tests/bench/results.json
    just perf-charts

# Reads tests/k6/baselines/*.json and tests/bench/results.json. The SVGs are
# generated artifacts that still have to be committed, since a README image
# cannot be built on demand by whoever is reading it.
#
# Redraw docs/perf/*.svg, the README's performance graphics.
perf-charts:
    go run ./tools/perfchart

# Fails when the committed charts no longer match the committed numbers, which
# is what moving a baseline without a redraw leaves behind.
#
# Strict read-only check that docs/perf is in sync.
perf-charts-check:
    #!/usr/bin/env bash
    set -euo pipefail
    go run ./tools/perfchart > /dev/null
    if [ -n "$(git status --porcelain docs/perf)" ]; then
        echo "::error::docs/perf is stale. Run 'just perf-charts' and commit."
        git --no-pager diff --stat docs/perf
        exit 1
    fi
    echo "performance charts in sync"

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
    # Catch up with the remote first. CI answers every push to the default
    # branch with a generated-artifact commit of its own, so a tree that was
    # in sync when you last pushed is behind by the time you release — and the
    # release's own push is then rejected non-fast-forward, after ten minutes
    # of checks. Tags come along so the version below counts from what is
    # actually released, not from what this machine happens to know.
    echo "Syncing with origin/$DEFAULT_BRANCH..."
    git fetch --tags --prune origin
    if [ -n "$(git rev-list HEAD..origin/$DEFAULT_BRANCH)" ]; then
        echo "origin/$DEFAULT_BRANCH has commits this tree does not; rebasing onto it."
        git pull --rebase --autostash origin "$DEFAULT_BRANCH"
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
    if git rev-parse -q --verify "refs/tags/$new" >/dev/null; then
        echo "Error: tag $new already exists here. Delete it (git tag -d $new) if it was never pushed." >&2
        exit 1
    fi
    echo "Bumping from $cur to $new"
    # The checks just committed the resynced artifacts, and they took long
    # enough that CI may have pushed again in the meantime. Land on whatever
    # is there and push the branch *first*.
    BRANCH=$(git rev-parse --abbrev-ref HEAD)
    git fetch origin "$BRANCH"
    if [ -n "$(git rev-list HEAD..FETCH_HEAD)" ]; then
        git rebase FETCH_HEAD
    fi
    # GitHub skips *every* workflow for a push whose head commit message says
    # [skip ci] — including the tag push that is the entire release mechanism
    # here. CI's own artifact bump says exactly that, so a release with no
    # drift of its own tags one of those commits and publishes nothing: no
    # binaries, no image, no release notes, and no failure either. Give the
    # tag a commit that runs.
    if git log -1 --format=%B HEAD | grep -qiE '\[skip ci\]|\[ci skip\]'; then
        echo "HEAD says [skip ci]; adding a release commit so the tag builds."
        git commit --allow-empty -m "chore: release $new"
    fi
    git push origin HEAD
    # Tag only once the branch is up. A tag made before a rejected push is a
    # version number spent for nothing: the next run counts from it and
    # silently skips a release number (v1.2.0 gone, v1.2.1 out instead).
    git tag -a "$new" -m "Release $new"
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
