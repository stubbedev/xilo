#!/usr/bin/env bash
# Chaos test: kill -9 the server mid-push, restart, prove no corruption —
# the interrupted path is absent (never half-registered), a re-push completes,
# and every previously pushed NAR still reassembles byte-exact via nix copy.
#
#   ./tests/e2e/chaos.sh        # needs: docker compose, go, nix
set -uo pipefail
cd "$(dirname "$0")/../.."

COMPOSE="docker compose -f tests/e2e/compose.yaml"
URL=http://127.0.0.1:18080
WORK=$(mktemp -d)
FAILS=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

cleanup() {
  $COMPOSE down -v >/dev/null 2>&1
  chmod -R +w "$WORK" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

go build -o "$WORK/xilo" ./cmd/xilo || exit 1
XILO="$WORK/xilo"
$COMPOSE up -d --build xilo >/dev/null 2>&1 || exit 1
for i in $(seq 1 60); do curl -fs $URL/healthz >/dev/null 2>&1 && break; sleep 1; done
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XILO_URL=$URL
mkdir -p "$XDG_CONFIG_HOME"
$COMPOSE exec -T xilo /xilo cache create chaos >/dev/null 2>&1
XILO_TOKEN=$($COMPOSE exec -T xilo /xilo token create chaos --push --pull 2>/dev/null | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
[ -n "$XILO_TOKEN" ] || { echo "token create failed"; exit 1; }
export XILO_TOKEN

echo "== baseline closure push =="
BASELINE=$(realpath "$(command -v bash)" 2>/dev/null | grep -oE '/nix/store/[^/]+')
[ -n "$BASELINE" ] || BASELINE=$(nix build nixpkgs#hello --no-link --print-out-paths 2>/dev/null | head -1)
[ -n "$BASELINE" ] || { echo "no pushable store path found"; exit 1; }
"$XILO" push chaos "$BASELINE" --quiet && pass "baseline push" || fail "baseline push"

echo "== kill -9 mid-push =="
head -c 200M /dev/urandom > "$WORK/victim.bin"
VICTIM=$(nix store add-path "$WORK/victim.bin" --name chaos-victim)
( "$XILO" push chaos "$VICTIM" --quiet >/dev/null 2>&1 ) &
PUSH_PID=$!
sleep 1.5
$COMPOSE kill -s SIGKILL xilo >/dev/null 2>&1 && pass "server SIGKILLed mid-push" || fail "server SIGKILLed mid-push"
wait $PUSH_PID 2>/dev/null # push fails, expected

echo "== restart + integrity =="
$COMPOSE up -d xilo >/dev/null 2>&1
for i in $(seq 1 60); do curl -fs $URL/healthz >/dev/null 2>&1 && break; sleep 1; done
curl -fs $URL/healthz >/dev/null && pass "server restarts on same data" || fail "server restarts on same data"

VH=${VICTIM#/nix/store/}; VH=${VH%%-*}
if curl -fs -o /dev/null "$URL/chaos/$VH.narinfo"; then
  # If it IS registered, it must reassemble byte-exact — verify.
  nix copy --no-check-sigs --from "$URL/chaos" --to "local?root=$WORK/v" "$VICTIM" >/dev/null 2>&1 \
    && pass "interrupted path registered AND intact" || fail "interrupted path registered but broken"
else
  pass "interrupted path not registered (no half-path)"
fi

"$XILO" push chaos "$VICTIM" --quiet && pass "re-push after crash" || fail "re-push after crash"
nix copy --no-check-sigs --from "$URL/chaos" --to "local?root=$WORK/nixroot" "$VICTIM" >/dev/null 2>&1 \
  && pass "victim pulls byte-exact (nix-verified)" || fail "victim pulls byte-exact"
nix copy --no-check-sigs --from "$URL/chaos" --to "local?root=$WORK/nixroot" "$BASELINE" >/dev/null 2>&1 \
  && pass "baseline closure intact after crash" || fail "baseline closure intact after crash"

echo "== gc after crash leaves no dangling state =="
$COMPOSE exec -T xilo /xilo gc | grep -qE "removed [0-9]+ chunks" && pass "gc runs" || fail "gc runs"
nix copy --no-check-sigs --from "$URL/chaos" --to "local?root=$WORK/nixroot2" "$VICTIM" >/dev/null 2>&1 \
  && pass "victim intact after post-crash gc" || fail "victim intact after post-crash gc"

echo
[ "$FAILS" -gt 0 ] && { echo "$FAILS FAILURES"; exit 1; }
echo "ALL PASS"
