#!/usr/bin/env bash
# Head-to-head: xilo vs attic on identical hardware, storage (docker volume,
# local backend, sqlite), chunking parameters, and workload. Produces the
# numbers behind "lower resource consumption than attic":
#
#   - cold push wall time (real nix closure, host client)
#   - dedup re-push wall time
#   - pull load: narinfo p95 / QPS, NAR throughput (same k6 script, both)
#   - server memory (max RSS) and CPU time consumed during the pull phase
#   - image size
#
#   ./tests/bench/bench.sh          # needs: docker, go, nix, ~5 min
#
# Fairness: sequential runs on an idle machine; attic is the latest published
# image; xilo is built from HEAD; both use zstd at rest and the same
# chunk min/avg/max + NAR threshold (see attic.toml here vs xilo defaults).
set -uo pipefail
cd "$(dirname "$0")/../.."

WORK=$(mktemp -d)
BENCH_DIR=tests/bench
XILO_URL=http://127.0.0.1:18080
ATTIC_URL=http://127.0.0.1:28080

cleanup() {
  docker rm -f bench-xilo bench-attic >/dev/null 2>&1
  docker volume rm bench-xilo-data bench-attic-data >/dev/null 2>&1
  chmod -R +w "$WORK" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== build + start xilo =="
go build -o "$WORK/xilo" ./cmd/xilo || exit 1
docker build -q -t xilo:bench . >/dev/null || exit 1
docker run -d --name bench-xilo -p 127.0.0.1:18080:8080 \
  -v bench-xilo-data:/data -e XILO_ADMIN_PASSWORD=bench xilo:bench >/dev/null
for i in $(seq 1 30); do curl -fs $XILO_URL/healthz >/dev/null 2>&1 && break; sleep 1; done

echo "== start attic =="
SECRET=$(head -c 64 /dev/urandom | base64 -w0)
sed "s|@SECRET@|$SECRET|" $BENCH_DIR/attic.toml > "$WORK/attic.toml"
docker run -d --name bench-attic -p 127.0.0.1:28080:8080 \
  -v "$WORK/attic.toml":/attic/server.toml:ro -v bench-attic-data:/data \
  ghcr.io/zhaofengli/attic:latest --config /attic/server.toml >/dev/null
for i in $(seq 1 30); do curl -fs $ATTIC_URL/ >/dev/null 2>&1 && break; sleep 1; done

export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config"
mkdir -p "$XDG_CONFIG_HOME"

echo "== provision =="
docker exec bench-xilo /xilo cache create bench >/dev/null
XILO_TOKEN=$(docker exec bench-xilo /xilo token create bench --cache bench --push --pull | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
export XILO_TOKEN XILO_URL
ATTIC_TOKEN=$(docker exec bench-attic atticadm make-token --sub bench --validity 1y \
  --pull '*' --push '*' --create-cache '*' --configure-cache '*' \
  --configure-cache-retention '*' --destroy-cache '*' --delete '*' \
  --config /attic/server.toml | tail -1)
ATTIC="nix shell nixpkgs#attic-client -c attic"
$ATTIC login bench $ATTIC_URL "$ATTIC_TOKEN" || { echo "attic login failed"; exit 1; }
$ATTIC cache create bench:bench || { echo "attic cache create failed"; exit 1; }
# --upstream-cache-key-name '': attic defaults to skipping nixpkgs-signed
# paths on push; xilo's default is to push everything (upstream_keys: []).
# Disable the filter so both push the identical closure.
$ATTIC cache configure bench:bench --public --upstream-cache-key-name '' || { echo "attic cache configure failed"; exit 1; }

# Workload: the combined closure of several common tools (~90 paths, ~800MB)
# — a realistic CI push.
ROOTS=""
for b in go git curl jq rg python3; do
  p=$(realpath "$(command -v $b)" 2>/dev/null | grep -oE '/nix/store/[^/]+') && ROOTS="$ROOTS $p"
done
nix path-info -r $ROOTS | sort -u > "$WORK/closure.txt"
sed -E 's|/nix/store/([a-z0-9]{32})-.*|\1|' "$WORK/closure.txt" > "$WORK/hashes.txt"
N=$(wc -l < "$WORK/closure.txt")
SZ=$(nix path-info -S $ROOTS | awk '{s+=$2} END {printf "%.0f", s/1e6}')
echo "closure: $N paths, ${SZ}MB"

t() { # wall-clock seconds, no external `time` binary (absent on NixOS)
  local s e
  s=$(date +%s.%N)
  "$@" >/dev/null 2>&1
  e=$(date +%s.%N)
  awk -v a="$s" -v b="$e" 'BEGIN{printf "%.1f", b-a}'
}

echo "== push (cold) =="
XILO_COLD=$(t "$WORK/xilo" push bench - --quiet < "$WORK/closure.txt")
ATTIC_COLD=$(t $ATTIC push bench:bench --stdin < "$WORK/closure.txt")
echo "== push (dedup re-push) =="
XILO_DEDUP=$(t "$WORK/xilo" push bench - --quiet < "$WORK/closure.txt")
ATTIC_DEDUP=$(t $ATTIC push bench:bench --stdin < "$WORK/closure.txt")
# sanity: both servers must actually hold the closure before the pull phase
SAMPLE=$(head -1 "$WORK/hashes.txt")
curl -fs "$XILO_URL/bench/$SAMPLE.narinfo" >/dev/null || { echo "xilo push did not land"; exit 1; }
curl -fs "$ATTIC_URL/bench/$SAMPLE.narinfo" >/dev/null || { echo "attic push did not land"; exit 1; }

sample_stats() { # sample_stats <container> <outfile> — until killed
  while true; do
    docker stats --no-stream --format "{{.MemUsage}} {{.CPUPerc}}" "$1" 2>/dev/null
    sleep 1
  done > "$2"
}

pull_bench() { # pull_bench <name> <container> <url>
  local name=$1 ctr=$2 url=$3
  sample_stats "$ctr" "$WORK/$name.stats" &
  local SAMPLER=$!
  docker run --rm --network host --user 0:0 -v "$PWD/$BENCH_DIR":/bench -v "$WORK":/work \
    grafana/k6:0.57.0 run -q --summary-export=/work/$name-pull.json \
    -e BASE_URL=$url/bench -e HASHES=/work/hashes.txt /bench/pull.js > "$WORK/$name-k6.log" 2>&1 \
    || echo "k6 $name failed — tail of log:" && tail -3 "$WORK/$name-k6.log" >&2
  kill $SAMPLER 2>/dev/null
}

echo "== pull load: xilo =="
pull_bench xilo bench-xilo $XILO_URL
echo "== pull load: attic =="
pull_bench attic bench-attic $ATTIC_URL

report() { # report <name>
  local f="$WORK/$1-pull.json"
  python3 - "$f" "$WORK/$1.stats" <<'PY'
import json, re, sys
s = json.load(open(sys.argv[1]))
m = s["metrics"]
def g(k, f, d=0): return m.get(k, {}).get(f, d)
qps = g("http_reqs", "rate")
p95 = g("http_req_duration{name:narinfo}", "p(95)")
narp95 = g("http_req_duration{name:nar}", "p(95)")
mbs = g("data_received", "rate") / 1e6
fail = g("http_req_failed", "value")
mem = []
cpu = []
for line in open(sys.argv[2]):
    mm = re.match(r"([\d.]+)(\w+) / \S+ ([\d.]+)%", line)
    if mm:
        v, unit, c = float(mm.group(1)), mm.group(2), float(mm.group(3))
        mem.append(v * {"KiB": 1/1024, "MiB": 1, "GiB": 1024}.get(unit, 1))
        cpu.append(c)
print(f"qps={qps:.0f} narinfo_p95={p95:.1f}ms nar_p95={narp95:.1f}ms pull={mbs:.0f}MB/s "
      f"fail={fail*100:.2f}% maxRSS={max(mem):.0f}MiB avgCPU={sum(cpu)/len(cpu):.0f}%")
PY
}

XILO_IMG=$(docker image inspect xilo:bench --format '{{.Size}}' | awk '{printf "%.0f", $1/1e6}')
ATTIC_IMG=$(docker image inspect ghcr.io/zhaofengli/attic:latest --format '{{.Size}}' | awk '{printf "%.0f", $1/1e6}')

echo
echo "================ RESULTS (closure: $N paths, ${SZ}MB) ================"
printf "%-22s %-30s %s\n" "metric" "xilo" "attic"
printf "%-22s %-30s %s\n" "cold push" "${XILO_COLD}s" "${ATTIC_COLD}s"
printf "%-22s %-30s %s\n" "dedup re-push" "${XILO_DEDUP}s" "${ATTIC_DEDUP}s"
printf "%-22s %-30s %s\n" "image size" "${XILO_IMG}MB" "${ATTIC_IMG}MB"
printf "%-22s %s\n" "pull (xilo)" "$(report xilo)"
printf "%-22s %s\n" "pull (attic)" "$(report attic)"
echo "======================================================================"
