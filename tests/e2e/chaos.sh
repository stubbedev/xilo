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
cd "$(dirname "$0")/../.." || exit 1

COMPOSE=(docker compose -f tests/e2e/compose.yaml)
URL=http://127.0.0.1:18080
WORK=$(mktemp -d)
FAILS=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

check() {
  local name=$1
  shift
  if "$@"; then pass "$name"; else fail "$name"; fi
}

cleanup() {
  "${COMPOSE[@]}" down -v >/dev/null 2>&1
  chmod -R +w "$WORK" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

wait_healthy() {
  for _ in $(seq 1 60); do curl -fs $URL/healthz >/dev/null 2>&1 && return 0; sleep 1; done
  return 1
}

start_server() {
  "${COMPOSE[@]}" up -d "$@" xilo || { echo "compose up -d $* xilo failed" >&2; return 1; }
  wait_healthy && return 0
  echo "server never became healthy at $URL/healthz" >&2
  "${COMPOSE[@]}" ps -a >&2
  "${COMPOSE[@]}" logs --tail 50 xilo >&2
  return 1
}

# mint_token <token-name> <cache-ref> — creates a push+pull token server-side
# and prints the secret.
mint_token() {
  "${COMPOSE[@]}" exec -T xilo /xilo token create "$1" --cache "$2" --push --pull 2>/dev/null \
    | grep -oE '[A-Za-z0-9_-]{40,}' | head -1
}

chunk_upload_seen() {
  timeout 60 grep -m1 -q " /c/$1/api/chunk/" < <(docker logs -f --since "$3" "$2" 2>&1)
}

exited_by_sigkill() {
  [ "$(timeout 30 docker wait "$1")" = 137 ]
}

push_failed() {
  ! wait "$1"
}

pull() {
  nix copy --no-check-sigs --from "$1" --to "local?root=$2" "$3" >/dev/null 2>&1
}

gc_removes_chunks() {
  local out
  out=$("${COMPOSE[@]}" exec -T xilo /xilo gc) || return 1
  grep -qE "removed [0-9]+ chunks" <<<"$out"
}

register_open() {
  curl -fs -o /dev/null "$URL/register"
}

register_closed() {
  ! register_open
}

# chaos_round <cache-ref> <label> — the full kill-9 invariant suite against
# /c/<cache-ref>. Expects XILO_TOKEN scoped to that cache.
chaos_round() {
  local ref=$1 label=$2 sub="$URL/c/$1"

  check "$label: baseline push" "$XILO" push "$ref" "$BASELINE" --quiet

  # Fresh random victim per round so its chunks never dedup against an
  # earlier round — the kill must land mid-upload, not mid-noop.
  head -c 200M /dev/urandom > "$WORK/victim-$label.bin"
  local victim
  victim=$(nix store add-path "$WORK/victim-$label.bin" --name "chaos-victim-$label")

  local cid since
  cid=$("${COMPOSE[@]}" ps -q xilo)
  since=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
  ( "$XILO" push "$ref" "$victim" --quiet >/dev/null 2>&1 ) &
  local push_pid=$!
  check "$label: victim chunks reach the server" chunk_upload_seen "$ref" "$cid" "$since"
  check "$label: push in flight at kill" kill -0 "$push_pid"
  docker kill --signal KILL "$cid" >/dev/null 2>&1
  check "$label: server SIGKILLed mid-push" exited_by_sigkill "$cid"
  check "$label: push fails against a killed server" push_failed "$push_pid"

  check "$label: server restarts on same data" start_server

  local vh=${victim#/nix/store/}
  vh=${vh%%-*}
  if curl -fs -o /dev/null "$sub/$vh.narinfo"; then
    # If it IS registered, it must reassemble byte-exact — verify.
    check "$label: interrupted path registered AND intact" pull "$sub" "$WORK/$label-half" "$victim"
  else
    pass "$label: interrupted path not registered (no half-path)"
  fi

  check "$label: re-push after crash" "$XILO" push "$ref" "$victim" --quiet
  check "$label: victim pulls byte-exact (nix-verified)" pull "$sub" "$WORK/$label-root" "$victim"
  check "$label: baseline closure intact after crash" pull "$sub" "$WORK/$label-root" "$BASELINE"
  check "$label: gc runs" gc_removes_chunks
  check "$label: victim intact after post-crash gc" pull "$sub" "$WORK/$label-root2" "$victim"
}

go build -o "$WORK/xilo" ./cmd/xilo || { echo "go build ./cmd/xilo failed" >&2; exit 1; }
XILO="$WORK/xilo"
start_server --build || exit 1
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XILO_URL=$URL
mkdir -p "$XDG_CONFIG_HOME"

BASELINE=$(realpath "$(command -v bash)" 2>/dev/null | grep -oE '/nix/store/[^/]+')
[ -n "$BASELINE" ] || BASELINE=$(nix build nixpkgs#hello --no-link --print-out-paths 2>/dev/null | head -1)
[ -n "$BASELINE" ] || { echo "no pushable store path found"; exit 1; }

echo "== single-tenant round (default/chaos) =="
# The namespace is explicit because there is no implicit one: a bare `cache
# create chaos` lands in the superadmin's own workspace (admin/chaos), so
# every push below asked for a cache that was never created. An explicit
# namespace mints the account on the fly, which is what the mt round below
# has always done and why only this round broke.
"${COMPOSE[@]}" exec -T xilo /xilo cache create default/chaos >/dev/null 2>&1
XILO_TOKEN=$(mint_token chaos default/chaos)
[ -n "$XILO_TOKEN" ] || { echo "token create failed"; exit 1; }
export XILO_TOKEN
chaos_round default/chaos st

echo "== multi-tenant round (registered user's cache) =="
# Same data volume, server restarted with multi_tenant: true (config mount).
export XILO_E2E_CONFIG=server-mt.yaml
start_server || { echo "mt server never came up" >&2; exit 1; }

check "mt: /register closed until enabled" register_closed
JAR="$WORK/cookies"
curl -fs -c "$JAR" -o /dev/null -d username=admin -d password=e2e-admin $URL/admin/login || fail "mt: admin login"
# allow_registrations on; require_approval absent → off (registrants go active).
check "mt: registrations enabled via admin session" \
  curl -fs -b "$JAR" -o /dev/null -d allow_registrations=1 $URL/admin/settings/instance
check "mt: /register open" register_open

# Success = 303 into a fresh session; every failure re-renders the form (200).
REG_CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  -d username=chaosuser -d email=chaos@example.com -d password=chaospass123 $URL/register)
if [ "$REG_CODE" = "303" ]; then
  pass "mt: user registered (active + session)"
else
  fail "mt: user registered (got $REG_CODE)"
fi

check "mt: cache under user account" "${COMPOSE[@]}" exec -T xilo /xilo cache create chaosuser/chaos-mt
XILO_TOKEN=$(mint_token chaos-mt chaosuser/chaos-mt)
[ -n "$XILO_TOKEN" ] || { echo "mt token create failed"; exit 1; }
export XILO_TOKEN
chaos_round chaosuser/chaos-mt mt

echo
if [ "$FAILS" -gt 0 ]; then
  echo "$FAILS FAILURES"
  exit 1
fi
echo "ALL PASS"
