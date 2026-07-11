#!/usr/bin/env bash
# CLI end-to-end suite: every xilo subcommand exercised against a
# containerized server, with the real Nix store as the push source and
# `nix copy` (which verifies NarHash on substitution) as the pull proof.
#
#   ./tests/e2e/cli.sh          # needs: docker compose, go, nix
#
# Each assertion prints PASS/FAIL; any failure exits nonzero at the end.
set -uo pipefail
cd "$(dirname "$0")/../.."

COMPOSE="docker compose -f tests/e2e/compose.yaml"
URL=http://127.0.0.1:18080
WORK=$(mktemp -d)
FAILS=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }
assert() { # assert <name> <command...> — failure prints the command output
  local name=$1 out rc; shift
  out=$("$@" 2>&1); rc=$?
  if [ $rc -eq 0 ]; then pass "$name"; else
    fail "$name"
    echo "  cmd: $*" >&2
    echo "$out" | sed 's/^/  out: /' | head -5 >&2
  fi
}

# closure_root prints a store path with a real closure to push. Prefers a
# host binary that lives in /nix/store (dev machines); falls back to building
# nixpkgs#hello (CI runners, where /usr/bin/bash is not a store path).
closure_root() {
  local p
  p=$(realpath "$(command -v bash)" 2>/dev/null | grep -oE '/nix/store/[^/]+')
  [ -n "$p" ] && { echo "$p"; return; }
  nix build nixpkgs#hello --no-link --print-out-paths 2>/dev/null | head -1
}

cleanup() {
  $COMPOSE down -v >/dev/null 2>&1
  chmod -R +w "$WORK" 2>/dev/null # nix copy roots are read-only
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== build =="
go build -o "$WORK/xilo" ./cmd/xilo || { echo "build failed"; exit 1; }
XILO="$WORK/xilo"

echo "== server up =="
$COMPOSE up -d --build xilo >/dev/null 2>&1 || { echo "compose up failed"; exit 1; }
for i in $(seq 1 60); do curl -fs $URL/healthz >/dev/null 2>&1 && break; sleep 1; done
curl -fs $URL/healthz >/dev/null || { echo "server never became healthy"; exit 1; }
pass "server healthy"

# Isolate all client-side config/state.
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config"
mkdir -p "$XDG_CONFIG_HOME"
export XILO_URL=$URL

exec_srv() { $COMPOSE exec -T xilo /xilo "$@"; }
export COMPOSE
export -f exec_srv # bash -c assertions need it too

echo "== schema =="
"$XILO" schema dump --out "$WORK/schema.json" && python3 -c "import json;json.load(open('$WORK/schema.json'))" 2>/dev/null \
  && pass "schema dump valid json" || fail "schema dump valid json"

echo "== cache lifecycle (server-side CLI) =="
assert "cache create" exec_srv cache create e2e
assert "cache create private" exec_srv cache create e2e-priv --private
assert "cache list shows both" bash -c "exec_srv cache list | grep -q e2e-priv"
assert "cache info" bash -c "exec_srv cache info e2e | grep -q 'public key'"
assert "cache configure" exec_srv cache configure e2e --priority 30
assert "configure applied" bash -c "curl -fs $URL/default/e2e/nix-cache-info | grep -q 'Priority: 30'"
KEY_BEFORE=$(exec_srv cache info e2e | grep 'public key')
assert "cache rotate" exec_srv cache rotate e2e
KEY_AFTER=$(exec_srv cache info e2e | grep 'public key')
[ "$KEY_BEFORE" != "$KEY_AFTER" ] && pass "rotate changed key" || fail "rotate changed key"

echo "== tokens =="
TOK=$(exec_srv token create e2e-full --push --pull 2>/dev/null | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
[ -n "$TOK" ] && pass "token create prints secret" || fail "token create prints secret"
assert "token list" bash -c "exec_srv token list | grep -q e2e-full"
DEAD=$(exec_srv token create e2e-dead --push 2>/dev/null | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
DEAD_ID=$(exec_srv token list | grep e2e-dead | grep -oE '^[0-9 ]+' | tr -d ' ' | head -1)
assert "token revoke" exec_srv token revoke "$DEAD_ID"
export XILO_TOKEN=$TOK

echo "== login (saved client config) =="
assert "login saves config" "$XILO" login $URL --token "$TOK"
grep -q "$TOK" "$XDG_CONFIG_HOME/xilo/config.yaml" && pass "config file holds token" || fail "config file holds token"

echo "== push (real nix closure) =="
CLOSURE_ROOT=$(closure_root)
[ -n "$CLOSURE_ROOT" ] || { echo "no pushable store path found"; exit 1; }
echo "closure root: $CLOSURE_ROOT"
assert "push --dry-run" "$XILO" push e2e "$CLOSURE_ROOT" --dry-run
assert "push closure" "$XILO" push e2e "$CLOSURE_ROOT" --quiet
assert "re-push dedups" bash -c "\"$XILO\" push e2e \"$CLOSURE_ROOT\" 2>&1 | grep -q 'already cached'"
assert "push from stdin" bash -c "echo \"$CLOSURE_ROOT\" | \"$XILO\" push e2e - --quiet"

echo "== pull (nix verifies hashes itself) =="
H=${CLOSURE_ROOT#/nix/store/}; H=${H%%-*}
curl -fs "$URL/default/e2e/$H.narinfo" | grep -q "Sig: e2e:" && pass "narinfo signed" || fail "narinfo signed"
nix copy --no-check-sigs --from "$URL/default/e2e" --to "local?root=$WORK/nixroot" "$CLOSURE_ROOT" >/dev/null 2>&1 \
  && pass "nix copy substitutes from cache" || fail "nix copy substitutes from cache"
[ -e "$WORK/nixroot$CLOSURE_ROOT" ] && pass "copied path materialized" || fail "copied path materialized"

echo "== use (nix.conf managed block) =="
assert "use adds substituter" "$XILO" use e2e
grep -q "$URL/default/e2e" "$XDG_CONFIG_HOME/nix/nix.conf" && pass "nix.conf contains substituter" || fail "nix.conf contains substituter"
grep -q "e2e:" "$XDG_CONFIG_HOME/nix/nix.conf" && pass "nix.conf contains trusted key" || fail "nix.conf contains trusted key"
assert "use second cache accumulates" "$XILO" use e2e-priv
grep -q "machine 127.0.0.1:18080" "$HOME/.netrc" && pass "netrc entry for private cache" || fail "netrc entry for private cache"
assert "use --remove" "$XILO" use e2e --remove
grep -q "$URL/default/e2e " "$XDG_CONFIG_HOME/nix/nix.conf" && fail "substituter removed" || pass "substituter removed"
grep -q "$URL/default/e2e-priv" "$XDG_CONFIG_HOME/nix/nix.conf" && pass "sibling substituter kept" || fail "sibling substituter kept"

echo "== private cache pull auth =="
curl -fs -o /dev/null "$URL/default/e2e-priv/nix-cache-info" && fail "private anonymous rejected" || pass "private anonymous rejected"
curl -fs -o /dev/null -H "Authorization: Bearer $TOK" "$URL/default/e2e-priv/nix-cache-info" && pass "private token accepted" || fail "private token accepted"

echo "== watch (inotify auto-push) =="
if [ "$(uname -s)" = "Linux" ]; then
  head -c 1M /dev/urandom > "$WORK/watched.bin"
  ( "$XILO" watch e2e >/dev/null 2>&1 ) &
  WATCH_PID=$!
  sleep 1
  WPATH=$(nix store add-path "$WORK/watched.bin" --name e2e-watched 2>/dev/null)
  WH=${WPATH#/nix/store/}; WH=${WH%%-*}
  ok=""
  for i in $(seq 1 30); do
    curl -fs -o /dev/null "$URL/default/e2e/$WH.narinfo" && ok=1 && break
    sleep 1
  done
  kill $WATCH_PID 2>/dev/null
  [ -n "$ok" ] && pass "watch auto-pushed new store path" || fail "watch auto-pushed new store path"
else
  echo "SKIP: watch (Linux only)"
fi

echo "== gc =="
exec_srv cache destroy e2e-priv >/dev/null 2>&1 && fail "destroy without --yes refused" || pass "destroy without --yes refused"
assert "cache destroy" exec_srv cache destroy e2e-priv --yes
GC_OUT=$(exec_srv gc 2>&1)
echo "$GC_OUT" | grep -qE "removed [0-9]+ chunks" && pass "gc reports sweep" || fail "gc reports sweep"
# everything still pullable after gc
nix copy --no-check-sigs --from "$URL/default/e2e" --to "local?root=$WORK/nixroot2" "$CLOSURE_ROOT" >/dev/null 2>&1 \
  && pass "cache intact after gc" || fail "cache intact after gc"

echo
if [ "$FAILS" -gt 0 ]; then
  echo "$FAILS FAILURES"
  exit 1
fi
echo "ALL PASS"
