#!/usr/bin/env bash
# Head-to-head: xilo against the other self-hostable Nix binary caches, on one
# machine, with the same closure, the same storage class and the same load
# script. Produces the numbers behind the README's comparison charts.
#
#   ./tests/bench/bench.sh                       # every target, ~20 min
#   ./tests/bench/bench.sh --targets xilo,attic  # a subset
#   ./tests/bench/bench.sh --json results.json   # machine-readable output
#
# Needs: docker, go, nix (flakes), ~10 GB of disk, an idle machine.
#
# Targets:
#   xilo      this working tree, built from HEAD: sqlite + local chunk storage
#   attic     the latest published image: sqlite + local chunk storage
#   harmonia  harmonia-cache, serving the host /nix/store (pull only)
#   nixserve  nix-serve-ng, serving the host /nix/store (pull only)
#   s3        MinIO plus `nix copy --to s3://…`: a plain object-store cache
#
# harmonia 3.x ships two binaries and the obvious one is not the server:
# `bin/harmonia` forwards its arguments to `nix` (it is a client wrapper, and
# running it bare fails with "no subcommand specified"), while the cache is
# `bin/harmonia-cache`. Point this at the wrapper and the target looks broken
# rather than misconfigured.
#
# What is measured per target:
#   push      cold and repeat-push wall time, for targets that accept a push
#   pull      narinfo QPS and p95, NAR throughput and p95, from one k6 script
#   resources max RSS and mean CPU of the server process during the pull phase
#   storage   bytes kept on disk for that closure
#   image     container image size
#
# Fairness rules, because a comparison is only worth the conditions it states:
#   - Targets run sequentially, never side by side, so nothing shares CPU.
#   - Every server runs in a container and every resource number comes from
#     `docker stats` on that container, so RSS is measured one way for all.
#   - The pull load is tests/bench/pull.js against all four: it speaks the
#     plain binary-cache protocol, so no target gets a client of its own.
#   - xilo and attic get the same chunk min/avg/max and NAR threshold
#     (tests/bench/attic.toml versus xilo's defaults) and both store zstd.
#   - The s3 target keeps `nix copy`'s default per-NAR compression, so its NAR
#     throughput counts compressed bytes and is NOT comparable with the servers
#     that reassemble and serve identity NARs. It is flagged as such in the
#     JSON and left out of the throughput chart.
#   - harmonia and nix-serve-ng serve paths that are already in the host store,
#     so they have no push phase. Both answer with `Compression: none`, so
#     their byte rates are the same quantity as xilo's, and their stored bytes
#     are the closure's own footprint in /nix/store: nothing compressed and
#     nothing deduplicated, which is the trade they make and is worth charting
#     rather than leaving blank.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

TARGETS=xilo,attic,harmonia,nixserve,s3
JSON=""
BENCH_PKGS=${BENCH_PKGS:-"git curl jq python3 go"}
while [ $# -gt 0 ]; do
  case "$1" in
  --targets)
    TARGETS=$2
    shift 2
    ;;
  --json)
    JSON=$2
    shift 2
    ;;
  -h | --help)
    sed -n '2,44p' "$0"
    exit 0
    ;;
  *)
    echo "unknown argument: $1" >&2
    exit 2
    ;;
  esac
done

WORK=$(mktemp -d)
BENCH_DIR=tests/bench
METRICS="$WORK/metrics.tsv"
: > "$METRICS"

# Ports are picked at start rather than fixed: a bench that dies on "address
# already in use" halfway through has thrown away ten minutes of measurement,
# and a workstation running this has other things bound.
port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
pick_port() {
  local p=$1
  while ! port_free "$p"; do p=$((p + 1)); done
  echo "$p"
}
XILO_PORT=$(pick_port 18080)
ATTIC_PORT=$(pick_port 28080)
HARMONIA_PORT=$(pick_port 37080)
NIXSERVE_PORT=$(pick_port 38080)
MINIO_PORT=$(pick_port 39000)
MINIO_KEY=benchbenchbench
MINIO_SECRET=benchbenchbench

cleanup() {
  docker rm -f bench-xilo bench-attic bench-harmonia bench-nixserve bench-minio >/dev/null 2>&1
  docker volume rm bench-xilo-data bench-attic-data >/dev/null 2>&1
  # MinIO writes its data as root, so the work directory cannot be removed from
  # here; a container deletes what a container created.
  docker run --rm -v "$WORK":/w busybox:latest rm -rf /w/minio >/dev/null 2>&1
  chmod -R +w "$WORK" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

has_target() { case ",$TARGETS," in *",$1,"*) return 0 ;; *) return 1 ;; esac; }

# put <target> <key> <value> records one measurement.
put() { printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$METRICS"; }

# skip <target> <reason> records a target that could not be measured. A missing
# comparator is reported, never quietly dropped: a chart with a row absent and
# no reason reads as a comparison nobody ran.
skip() {
  echo "!! skipping $1: $2" >&2
  put "$1" status skipped
  put "$1" error "$2"
}

# t <cmd...> prints wall-clock seconds (no external `time`; absent on NixOS).
t() {
  local s e
  s=$(date +%s.%N)
  "$@" >/dev/null 2>&1
  e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" 'BEGIN{printf "%.1f", b-a}'
}

# wait_up <url> waits up to 60s for an HTTP endpoint to answer.
wait_up() {
  local _i
  for _i in $(seq 1 60); do
    curl -fs -o /dev/null "$1" && return 0
    sleep 1
  done
  return 1
}

# size_bytes <volume-or-path> sizes a docker volume or a host directory from a
# container, so the number does not depend on the host's du flags.
size_bytes() {
  docker run --rm -v "$1":/d:ro busybox:latest du -sb /d 2>/dev/null | awk '{print $1}'
}

image_bytes() {
  docker image inspect "$1" --format '{{.Size}}' 2>/dev/null || echo 0
}

# stage_nix_db copies the store database once per run. The servers that read
# the host store need it writable (a WAL reader creates its own -shm file), and
# the real /nix/var/nix/db is not something a benchmark should be handing to a
# container read-write.
stage_nix_db() {
  [ -f "$WORK/nixdb/db.sqlite" ] && return 0
  mkdir -p "$WORK/nixdb"
  cp -f /nix/var/nix/db/db.sqlite* "$WORK/nixdb/" 2>/dev/null || return 1
  chmod -R u+w "$WORK/nixdb"
}

# closure_bytes <store path> is what installing that package costs, which is
# the honest counterpart to a container image size for something deployed from
# nixpkgs rather than pulled as an image.
closure_bytes() {
  nix path-info -S "$1" 2>/dev/null | awk '{print $2}'
}

# closure_disk_bytes is the workload's own footprint in /nix/store, measured
# the same way as every cache's storage: busybox du in a container, hardlinks
# counted once. It is what the host-store servers hold for this closure, since
# they keep it unpacked and compress nothing, and it is the number every
# cache's stored bytes should be read against.
closure_disk_bytes() {
  docker run --rm -v /nix/store:/nix/store:ro -v "$WORK":/w:ro busybox:latest \
    sh -c 'du -scb $(cat /w/closure.txt) 2>/dev/null | tail -1' | awk '{print $1}'
}

# ── workload ────────────────────────────────────────────────────────────────
# A fixed package set resolved through the flake's locked nixpkgs, so the
# closure is the same shape on a laptop and on a CI runner (where none of
# these tools comes from /nix/store).
echo "== resolve workload =="
ROOTS=""
for p in $BENCH_PKGS; do
  out=$(nix build --inputs-from . --no-link --print-out-paths "nixpkgs#$p" 2>/dev/null | head -1)
  [ -n "$out" ] && ROOTS="$ROOTS $out"
done
if [ -z "$ROOTS" ]; then
  echo "could not resolve any of: $BENCH_PKGS" >&2
  exit 1
fi
# shellcheck disable=SC2086 # ROOTS is a deliberate word list
nix path-info -r $ROOTS | sort -u > "$WORK/closure.txt"
sed -E 's|/nix/store/([a-z0-9]{32})-.*|\1|' "$WORK/closure.txt" > "$WORK/hashes.txt"
N=$(wc -l < "$WORK/closure.txt")
# The workload's real size is the sum of the closure's NarSizes. `path-info -S`
# per root is a closure size each, so adding those up counts every shared
# dependency once per root: the old number was half again too big.
# shellcheck disable=SC2046 # the closure list is a deliberate word list
NAR_TOTAL=$(nix path-info --json $(cat "$WORK/closure.txt") 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
rows = list(d.values()) if isinstance(d, dict) else d
print(sum(r["narSize"] for r in rows if r))
')
echo "closure: $N paths, $((NAR_TOTAL / 1000000))MB of NAR (roots: $BENCH_PKGS)"
put workload paths "$N"
put workload nar_bytes "${NAR_TOTAL:-0}"
put workload megabytes "$((NAR_TOTAL / 1000000))"
put workload packages "$BENCH_PKGS"
CLOSURE_DISK=$(closure_disk_bytes)
put workload store_bytes "${CLOSURE_DISK:-0}"

# ── load phase ──────────────────────────────────────────────────────────────
sample_stats() { # sample_stats <container> <outfile> — until killed
  while true; do
    docker stats --no-stream --format "{{.MemUsage}} {{.CPUPerc}}" "$1" 2>/dev/null
    sleep 1
  done > "$2"
}

pull_bench() { # pull_bench <target> <container> <cache-url>
  local name=$1 ctr=$2 url=$3
  sample_stats "$ctr" "$WORK/$name.stats" &
  local sampler=$!
  docker run --rm --network host --user 0:0 -v "$PWD/$BENCH_DIR":/bench -v "$WORK":/work \
    grafana/k6:0.57.0 run -q --summary-export="/work/$name-pull.json" \
    -e "BASE_URL=$url" -e HASHES=/work/hashes.txt /bench/pull.js > "$WORK/$name-k6.log" 2>&1
  local rc=$?
  kill $sampler 2>/dev/null
  if [ $rc -ne 0 ]; then
    echo "!! k6 exited $rc for $name; tail of log:" >&2
    tail -5 "$WORK/$name-k6.log" >&2
  fi
  python3 - "$WORK/$name-pull.json" "$WORK/$name.stats" "$name" "$METRICS" <<'PY'
import json, re, sys
summary, stats, name, out = sys.argv[1:5]
m = json.load(open(summary))["metrics"]
def g(k, f, d=0.0):
    return m.get(k, {}).get(f, d)
rows = {
    "narinfo_qps": g("http_reqs", "rate"),
    "narinfo_p95_ms": g("http_req_duration{name:narinfo}", "p(95)"),
    "nar_p95_ms": g("http_req_duration{name:nar}", "p(95)"),
    "pull_mbs": g("data_received", "rate") / 1e6,
    "fail_pct": g("http_req_failed", "value") * 100,
}
mem, cpu = [], []
unit = {"KiB": 1 / 1024, "MiB": 1, "GiB": 1024, "B": 1 / (1024 * 1024)}
for line in open(stats):
    mm = re.match(r"([\d.]+)(\w+) / \S+ ([\d.]+)%", line)
    if mm:
        mem.append(float(mm.group(1)) * unit.get(mm.group(2), 1))
        cpu.append(float(mm.group(3)))
if mem:
    rows["max_rss_mib"] = max(mem)
    rows["mean_cpu_pct"] = sum(cpu) / len(cpu)
with open(out, "a") as f:
    for k, v in rows.items():
        f.write(f"{name}\t{k}\t{v:.3f}\n")
PY
}

# ── xilo ────────────────────────────────────────────────────────────────────
run_xilo() {
  echo "== xilo: build + start =="
  go build -o "$WORK/xilo" ./cmd/xilo || {
    skip xilo "go build failed"
    return
  }
  docker build -q -t xilo:bench . >/dev/null || {
    skip xilo "docker build failed"
    return
  }
  docker run -d --name bench-xilo -p "127.0.0.1:$XILO_PORT:8080" \
    -v bench-xilo-data:/data -e XILO_ADMIN_PASSWORD=bench xilo:bench >/dev/null
  local url=http://127.0.0.1:$XILO_PORT
  wait_up "$url/healthz" || {
    skip xilo "healthz never came up"
    return
  }
  put xilo version "$(docker exec bench-xilo /xilo --version 2>/dev/null | head -1)"
  docker exec bench-xilo /xilo cache create bench/bench >/dev/null || {
    skip xilo "cache create failed"
    return
  }
  local token
  token=$(docker exec bench-xilo /xilo token create bench --cache bench/bench --push --pull |
    grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
  export XILO_URL=$url XILO_TOKEN=$token

  echo "== xilo: push =="
  put xilo cold_push_s "$(t "$WORK/xilo" push bench/bench - --quiet < "$WORK/closure.txt")"
  put xilo repeat_push_s "$(t "$WORK/xilo" push bench/bench - --quiet < "$WORK/closure.txt")"
  unset XILO_URL XILO_TOKEN

  local sample
  sample=$(head -1 "$WORK/hashes.txt")
  curl -fs -o /dev/null "$url/c/bench/bench/$sample.narinfo" || {
    skip xilo "push did not land (no narinfo for $sample)"
    return
  }
  echo "== xilo: pull load =="
  pull_bench xilo bench-xilo "$url/c/bench/bench"
  put xilo stored_bytes "$(size_bytes bench-xilo-data)"
  put xilo storage_kind cache
  put xilo image_bytes "$(image_bytes xilo:bench)"
  put xilo nar_comparable true
  put xilo status ok
  docker rm -f bench-xilo >/dev/null 2>&1
}

# ── attic ───────────────────────────────────────────────────────────────────
run_attic() {
  echo "== attic: start =="
  local secret
  secret=$(head -c 64 /dev/urandom | base64 -w0)
  sed "s|@SECRET@|$secret|" "$BENCH_DIR/attic.toml" > "$WORK/attic.toml"
  docker pull -q ghcr.io/zhaofengli/attic:latest >/dev/null 2>&1
  docker run -d --name bench-attic -p "127.0.0.1:$ATTIC_PORT:8080" \
    -v "$WORK/attic.toml":/attic/server.toml:ro -v bench-attic-data:/data \
    ghcr.io/zhaofengli/attic:latest --config /attic/server.toml >/dev/null
  local url=http://127.0.0.1:$ATTIC_PORT
  wait_up "$url/" || {
    skip attic "server never came up"
    return
  }
  local token
  token=$(docker exec bench-attic atticadm make-token --sub bench --validity 1y \
    --pull '*' --push '*' --create-cache '*' --configure-cache '*' \
    --configure-cache-retention '*' --destroy-cache '*' --delete '*' \
    --config /attic/server.toml | tail -1)
  local attic="nix shell --inputs-from . nixpkgs#attic-client -c attic"
  $attic login bench "$url" "$token" >/dev/null 2>&1 || {
    skip attic "attic login failed"
    return
  }
  $attic cache create bench:bench >/dev/null 2>&1 || {
    skip attic "attic cache create failed"
    return
  }
  # attic skips nixpkgs-signed paths on push by default and xilo pushes
  # everything (upstream_keys: []); clearing the filter makes both push the
  # identical closure.
  $attic cache configure bench:bench --public --upstream-cache-key-name '' >/dev/null 2>&1 || {
    skip attic "attic cache configure failed"
    return
  }

  echo "== attic: push =="
  put attic cold_push_s "$(t $attic push bench:bench --stdin < "$WORK/closure.txt")"
  put attic repeat_push_s "$(t $attic push bench:bench --stdin < "$WORK/closure.txt")"

  local sample
  sample=$(head -1 "$WORK/hashes.txt")
  curl -fs -o /dev/null "$url/bench/$sample.narinfo" || {
    skip attic "push did not land (no narinfo for $sample)"
    return
  }
  echo "== attic: pull load =="
  pull_bench attic bench-attic "$url/bench"
  put attic stored_bytes "$(size_bytes bench-attic-data)"
  put attic storage_kind cache
  put attic image_bytes "$(image_bytes ghcr.io/zhaofengli/attic:latest)"
  put attic nar_comparable true
  put attic status ok
  docker rm -f bench-attic >/dev/null 2>&1
}

# ── harmonia ────────────────────────────────────────────────────────────────
# Serves the host store, so there is nothing to push and nothing stored twice.
# Runs in a container like every other target, which is what makes its RSS
# comparable: the store is bind-mounted (read-write, because libstore remounts
# it at startup) and the store database is the staged copy.
run_harmonia() {
  echo "== harmonia: start =="
  local pkg
  pkg=$(nix build --inputs-from . --no-link --print-out-paths nixpkgs#harmonia 2>/dev/null | head -1)
  [ -n "$pkg" ] || {
    skip harmonia "nixpkgs#harmonia would not build"
    return
  }
  nix key generate-secret --key-name bench-harmonia > "$WORK/harmonia.key" 2>/dev/null || {
    skip harmonia "could not generate a signing key"
    return
  }
  stage_nix_db || {
    skip harmonia "cannot read /nix/var/nix/db"
    return
  }
  cat > "$WORK/harmonia.toml" <<EOF
bind = "[::]:5000"
workers = 4
priority = 30
sign_key_paths = ["/keys/harmonia.key"]
EOF
  # bin/harmonia is a wrapper around nix; bin/harmonia-cache is the server.
  docker run -d --name bench-harmonia -p "127.0.0.1:$HARMONIA_PORT:5000" \
    -v /nix/store:/nix/store -v "$WORK/nixdb":/nix/var/nix/db -v "$WORK":/keys:ro \
    -e CONFIG_FILE=/keys/harmonia.toml -e RUST_LOG=error \
    --entrypoint "$pkg/bin/harmonia-cache" busybox:latest >/dev/null
  local url=http://127.0.0.1:$HARMONIA_PORT
  wait_up "$url/nix-cache-info" || {
    echo "-- harmonia log --" >&2
    docker logs bench-harmonia 2>&1 | tail -5 >&2
    skip harmonia "server never came up"
    return
  }
  local sample
  sample=$(head -1 "$WORK/hashes.txt")
  curl -fs -o /dev/null "$url/$sample.narinfo" || {
    skip harmonia "no narinfo for a path in the host store"
    return
  }
  echo "== harmonia: pull load =="
  pull_bench harmonia bench-harmonia "$url"
  put harmonia image_bytes "$(closure_bytes "$pkg")"
  # Not a dash: serving the host store means the bytes for this closure are the
  # store paths themselves, uncompressed and undeduplicated. Free when the
  # cache is the machine that built them, the full amount when it is not.
  put harmonia stored_bytes "${CLOSURE_DISK:-0}"
  put harmonia storage_kind host-store
  put harmonia serves_host_store true
  put harmonia nar_comparable true
  put harmonia status ok
  docker rm -f bench-harmonia >/dev/null 2>&1
}

# ── nix-serve-ng ────────────────────────────────────────────────────────────
# Serves the host store, so there is nothing to push and nothing stored twice.
# It runs in a container like the others (its own /nix/store references resolve
# through the bind mount) so its RSS comes from the same `docker stats`
# sampler as everyone else's.
run_nixserve() {
  echo "== nix-serve-ng: start =="
  local pkg
  pkg=$(nix build --inputs-from . --no-link --print-out-paths nixpkgs#nix-serve-ng 2>/dev/null | head -1)
  [ -n "$pkg" ] || {
    skip nixserve "nixpkgs#nix-serve-ng would not build"
    return
  }
  nix key generate-secret --key-name bench-nixserve > "$WORK/nixserve.key" 2>/dev/null || {
    skip nixserve "could not generate a signing key"
    return
  }
  stage_nix_db || {
    skip nixserve "cannot read /nix/var/nix/db"
    return
  }
  docker run -d --name bench-nixserve -p "127.0.0.1:$NIXSERVE_PORT:5000" \
    -v /nix/store:/nix/store -v "$WORK/nixdb":/nix/var/nix/db -v "$WORK":/keys:ro \
    -e NIX_SECRET_KEY_FILE=/keys/nixserve.key \
    --entrypoint "$pkg/bin/nix-serve" busybox:latest --host 0.0.0.0 --port 5000 >/dev/null
  local url=http://127.0.0.1:$NIXSERVE_PORT
  wait_up "$url/nix-cache-info" || {
    echo "-- nix-serve log --" >&2
    docker logs bench-nixserve 2>&1 | tail -5 >&2
    skip nixserve "server never came up"
    return
  }
  local sample
  sample=$(head -1 "$WORK/hashes.txt")
  curl -fs -o /dev/null "$url/$sample.narinfo" || {
    skip nixserve "no narinfo for a path in the host store"
    return
  }
  echo "== nix-serve-ng: pull load =="
  pull_bench nixserve bench-nixserve "$url"
  # No image and no data directory of its own, so report what a deployment
  # actually installs: the package closure.
  put nixserve image_bytes "$(closure_bytes "$pkg")"
  put nixserve stored_bytes "${CLOSURE_DISK:-0}"
  put nixserve storage_kind host-store
  put nixserve serves_host_store true
  put nixserve nar_comparable true
  put nixserve status ok
  docker rm -f bench-nixserve >/dev/null 2>&1
}

# ── s3 (MinIO + nix copy) ───────────────────────────────────────────────────
run_s3() {
  echo "== s3: start MinIO =="
  mkdir -p "$WORK/minio/bench"
  docker pull -q minio/minio:latest >/dev/null 2>&1
  docker run -d --name bench-minio -p "127.0.0.1:$MINIO_PORT:9000" \
    -v "$WORK/minio":/data \
    -e "MINIO_ROOT_USER=$MINIO_KEY" -e "MINIO_ROOT_PASSWORD=$MINIO_SECRET" \
    minio/minio:latest server /data >/dev/null
  local url=http://127.0.0.1:$MINIO_PORT
  wait_up "$url/minio/health/live" || {
    skip s3 "MinIO never came up"
    return
  }
  # A binary cache is read anonymously, and a MinIO bucket is private until
  # told otherwise: without this the pull phase measures 403s.
  docker run --rm --network host --entrypoint sh minio/mc:latest -c "
    mc alias set bench http://127.0.0.1:$MINIO_PORT $MINIO_KEY $MINIO_SECRET >/dev/null &&
    mc anonymous set download bench/bench >/dev/null" || {
    skip s3 "could not open the bucket for anonymous reads"
    return
  }
  nix key generate-secret --key-name bench-s3 > "$WORK/s3.key" 2>/dev/null
  local store="s3://bench?endpoint=127.0.0.1:$MINIO_PORT&region=us-east-1&scheme=http&secret-key=$WORK/s3.key"
  export AWS_ACCESS_KEY_ID=$MINIO_KEY AWS_SECRET_ACCESS_KEY=$MINIO_SECRET
  # nix remembers per-store which paths a binary cache holds, in
  # ~/.cache/nix/binary-cache-v6.sqlite. The bucket is new on every run and
  # that memory is not, so without these TTLs the second run of the day copies
  # nothing, reports success, and leaves an empty bucket to load-test.
  export NIX_CONFIG="narinfo-cache-positive-ttl = 0
narinfo-cache-negative-ttl = 0"

  echo "== s3: nix copy =="
  # shellcheck disable=SC2086
  local cold
  cold=$(t nix copy --to "$store" $ROOTS)
  local sample
  sample=$(head -1 "$WORK/hashes.txt")
  if ! curl -fs -o /dev/null "$url/bench/$sample.narinfo"; then
    skip s3 "nix copy did not land (no narinfo for $sample)"
    unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY NIX_CONFIG
    return
  fi
  put s3 cold_push_s "$cold"
  # shellcheck disable=SC2086
  put s3 repeat_push_s "$(t nix copy --to "$store" $ROOTS)"
  unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY NIX_CONFIG

  echo "== s3: pull load =="
  pull_bench s3 bench-minio "$url/bench"
  put s3 stored_bytes "$(size_bytes "$WORK/minio")"
  put s3 storage_kind cache
  put s3 image_bytes "$(image_bytes minio/minio:latest)"
  # `nix copy` stores each NAR compressed and the bucket serves those bytes
  # verbatim, so the byte rate is not the same quantity the other targets'
  # identity NARs report.
  put s3 nar_comparable false
  put s3 status ok
  docker rm -f bench-minio >/dev/null 2>&1
}

for target in xilo attic harmonia nixserve s3; do
  has_target "$target" && "run_$target"
done

# ── report ──────────────────────────────────────────────────────────────────
RUNNER=${BENCH_RUNNER:-"$(uname -s) $(uname -m), $(nproc) cores"}
python3 - "$METRICS" "$RUNNER" "${JSON:-}" <<'PY'
import json, os, subprocess, sys, time
metrics, runner, out = sys.argv[1], sys.argv[2], sys.argv[3]
data = {}
for line in open(metrics):
    target, key, value = line.rstrip("\n").split("\t", 2)
    try:
        value = float(value)
        if value == int(value) and key.endswith(("_bytes", "paths", "megabytes")):
            value = int(value)
    except ValueError:
        pass
    data.setdefault(target, {})[key] = value

def rev():
    try:
        return subprocess.run(["git", "describe", "--tags", "--always", "--dirty"],
                              capture_output=True, text=True, check=True).stdout.strip()
    except Exception:
        return "unknown"

doc = {
    "_comment": "Generated by tests/bench/bench.sh. Rendered into the README charts by `just perf-charts`.",
    "date": time.strftime("%Y-%m-%d"),
    "runner": runner,
    "xilo_rev": rev(),
    "workload": data.pop("workload", {}),
    "targets": data,
}

names = {"xilo": "xilo", "attic": "attic", "harmonia": "harmonia",
         "nixserve": "nix-serve-ng", "s3": "MinIO + nix copy"}
def fmt(t, key, unit="", scale=1.0, nd=1):
    v = data.get(t, {}).get(key)
    if v is None:
        return "-"
    if isinstance(v, str):  # storage_kind and friends are words, not figures
        return v
    return f"{v / scale:.{nd}f}{unit}"

rows = [
    ("cold push", "cold_push_s", "s", 1.0, 1),
    ("repeat push", "repeat_push_s", "s", 1.0, 1),
    ("narinfo QPS", "narinfo_qps", "", 1.0, 0),
    ("narinfo p95", "narinfo_p95_ms", "ms", 1.0, 1),
    ("NAR p95", "nar_p95_ms", "ms", 1.0, 0),
    ("pull throughput", "pull_mbs", "MB/s", 1.0, 0),
    ("max RSS", "max_rss_mib", "MiB", 1.0, 0),
    ("mean CPU", "mean_cpu_pct", "%", 1.0, 0),
    ("stored on disk", "stored_bytes", "MB", 1e6, 0),
    ("storage is", "storage_kind", "", 1.0, 0),
    ("image / closure", "image_bytes", "MB", 1e6, 0),
]
present = [t for t in ("xilo", "attic", "harmonia", "nixserve", "s3") if t in data]
w = doc["workload"]
print()
print(f"===== {w.get('paths', '?')} paths, {w.get('megabytes', '?')}MB, {runner} =====")
head = f"{'metric':<18}" + "".join(f"{names[t]:>20}" for t in present)
print(head)
for label, key, unit, scale, nd in rows:
    print(f"{label:<18}" + "".join(f"{fmt(t, key, unit, scale, nd):>20}" for t in present))
for t in present:
    if data[t].get("status") != "ok":
        print(f"note: {names[t]} {data[t].get('status')}: {data[t].get('error', '')}")
    elif data[t].get("nar_comparable") == "false":
        print(f"note: {names[t]} serves compressed NARs; its byte rate is not comparable")
print("=" * len(head))

if out:
    with open(out, "w") as f:
        json.dump(doc, f, indent=2, sort_keys=True)
        f.write("\n")
    print(f"wrote {out}")
PY
