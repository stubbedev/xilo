#!/usr/bin/env bash
# Chaos test: kill -9 the server mid-push, restart, prove no corruption —
# the interrupted path is absent (never half-registered), a re-push completes,
# and every previously pushed NAR still reassembles byte-exact via nix copy.
#
# Runs the same invariant suite twice: single-tenant (default/chaos), then
# multi-tenant (server restarted with multi_tenant: true, registrations
# enabled via the admin API, a self-registered user's cache at /c/{user}/…).
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

wait_healthy() {
  for i in $(seq 1 60); do curl -fs $URL/healthz >/dev/null 2>&1 && return 0; sleep 1; done
  return 1
}

# mint_token <token-name> <cache-ref> — creates a push+pull token server-side
# and prints the secret.
mint_token() {
  $COMPOSE exec -T xilo /xilo token create "$1" --cache "$2" --push --pull 2>/dev/null \
    | grep -oE '[A-Za-z0-9_-]{40,}' | head -1
}

# chaos_round <cache-ref> <label> — the full kill-9 invariant suite against
# /c/<cache-ref>. Expects XILO_TOKEN scoped to that cache.
chaos_round() {
  local ref=$1 label=$2 sub="$URL/c/$1"

  "$XILO" push "$ref" "$BASELINE" --quiet && pass "$label: baseline push" || fail "$label: baseline push"

  # Fresh random victim per round so its chunks never dedup against an
  # earlier round — the kill must land mid-upload, not mid-noop.
  head -c 200M /dev/urandom > "$WORK/victim-$label.bin"
  local victim
  victim=$(nix store add-path "$WORK/victim-$label.bin" --name "chaos-victim-$label")

  ( "$XILO" push "$ref" "$victim" --quiet >/dev/null 2>&1 ) &
  local push_pid=$!
  sleep 1.5
  $COMPOSE kill -s SIGKILL xilo >/dev/null 2>&1 && pass "$label: server SIGKILLed mid-push" || fail "$label: server SIGKILLed mid-push"
  wait $push_pid 2>/dev/null # push fails, expected

  $COMPOSE up -d xilo >/dev/null 2>&1
  wait_healthy && pass "$label: server restarts on same data" || fail "$label: server restarts on same data"

  local vh=${victim#/nix/store/}
  vh=${vh%%-*}
  if curl -fs -o /dev/null "$sub/$vh.narinfo"; then
    # If it IS registered, it must reassemble byte-exact — verify.
    nix copy --no-check-sigs --from "$sub" --to "local?root=$WORK/$label-half" "$victim" >/dev/null 2>&1 \
      && pass "$label: interrupted path registered AND intact" || fail "$label: interrupted path registered but broken"
  else
    pass "$label: interrupted path not registered (no half-path)"
  fi

  "$XILO" push "$ref" "$victim" --quiet && pass "$label: re-push after crash" || fail "$label: re-push after crash"
  nix copy --no-check-sigs --from "$sub" --to "local?root=$WORK/$label-root" "$victim" >/dev/null 2>&1 \
    && pass "$label: victim pulls byte-exact (nix-verified)" || fail "$label: victim pulls byte-exact"
  nix copy --no-check-sigs --from "$sub" --to "local?root=$WORK/$label-root" "$BASELINE" >/dev/null 2>&1 \
    && pass "$label: baseline closure intact after crash" || fail "$label: baseline closure intact after crash"

  $COMPOSE exec -T xilo /xilo gc | grep -qE "removed [0-9]+ chunks" && pass "$label: gc runs" || fail "$label: gc runs"
  nix copy --no-check-sigs --from "$sub" --to "local?root=$WORK/$label-root2" "$victim" >/dev/null 2>&1 \
    && pass "$label: victim intact after post-crash gc" || fail "$label: victim intact after post-crash gc"
}

go build -o "$WORK/xilo" ./cmd/xilo || exit 1
XILO="$WORK/xilo"
$COMPOSE up -d --build xilo >/dev/null 2>&1 || exit 1
wait_healthy || { echo "server never became healthy"; exit 1; }
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XILO_URL=$URL
mkdir -p "$XDG_CONFIG_HOME"

BASELINE=$(realpath "$(command -v bash)" 2>/dev/null | grep -oE '/nix/store/[^/]+')
[ -n "$BASELINE" ] || BASELINE=$(nix build nixpkgs#hello --no-link --print-out-paths 2>/dev/null | head -1)
[ -n "$BASELINE" ] || { echo "no pushable store path found"; exit 1; }

echo "== single-tenant round (default/chaos) =="
$COMPOSE exec -T xilo /xilo cache create chaos >/dev/null 2>&1
XILO_TOKEN=$(mint_token chaos chaos)
[ -n "$XILO_TOKEN" ] || { echo "token create failed"; exit 1; }
export XILO_TOKEN
chaos_round default/chaos st

echo "== multi-tenant round (registered user's cache) =="
# Same data volume, server restarted with multi_tenant: true (config mount).
export XILO_E2E_CONFIG=server-mt.yaml
$COMPOSE up -d xilo >/dev/null 2>&1
wait_healthy || { echo "mt server never became healthy"; exit 1; }

curl -fs -o /dev/null $URL/register && fail "mt: /register closed until enabled" || pass "mt: /register closed until enabled"
JAR="$WORK/cookies"
curl -fs -c "$JAR" -o /dev/null -d username=admin -d password=e2e-admin $URL/admin/login || fail "mt: admin login"
# allow_registrations on; require_approval absent → off (registrants go active).
curl -fs -b "$JAR" -o /dev/null -d allow_registrations=1 $URL/admin/settings/instance \
  && pass "mt: registrations enabled via admin session" || fail "mt: registrations enabled via admin session"
curl -fs -o /dev/null $URL/register && pass "mt: /register open" || fail "mt: /register open"

# Success = 303 into a fresh session; every failure re-renders the form (200).
REG_CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  -d username=chaosuser -d email=chaos@example.com -d password=chaospass123 $URL/register)
[ "$REG_CODE" = "303" ] && pass "mt: user registered (active + session)" || fail "mt: user registered (got $REG_CODE)"

$COMPOSE exec -T xilo /xilo cache create chaosuser/chaos-mt >/dev/null 2>&1 \
  && pass "mt: cache under user account" || fail "mt: cache under user account"
XILO_TOKEN=$(mint_token chaos-mt chaosuser/chaos-mt)
[ -n "$XILO_TOKEN" ] || { echo "mt token create failed"; exit 1; }
export XILO_TOKEN
chaos_round chaosuser/chaos-mt mt

echo
[ "$FAILS" -gt 0 ] && { echo "$FAILS FAILURES"; exit 1; }
echo "ALL PASS"
