#!/usr/bin/env bash
# Throwaway local instance with dummy data, for eyeballing the admin UI.
# Everything lives under ./tmp/demo-<port> (git-ignored) and is wiped on every run.
# Seeds: two caches (one private + capped), tokens, an org, a user, a plan, a
# real store-path closure pushed into each cache, and enough admin/API calls
# to fill the activities page. Ctrl-C stops the server.
#
#   just demo            # http://localhost:8090, admin / demo
#   just demo 8095       # another port
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=${1:-8090}
URL="http://localhost:$PORT"
WORK="$PWD/tmp/demo-$PORT"
XILO=./bin/xilo
ADMIN_PW=demo

rm -rf "$WORK" && mkdir -p "$WORK/data" "$WORK/home/.config"
cat > "$WORK/xilo.yaml" <<YAML
listen: ":$PORT"
base_url: "$URL"
data_dir: "$WORK/data"
self_service: true
admin:
  password: "$ADMIN_PW"
storage:
  local:
    root: "$WORK/storage"
YAML
export XILO_CONFIG="$WORK/xilo.yaml"
# Client-side state (the xilo login profile) stays out of the real ~/.config.
# Only XDG_CONFIG_HOME moves; HOME stays real so the browser launched at the
# end opens your own profile, not a blank one.
REAL_XDG="${XDG_CONFIG_HOME:-$HOME/.config}"
export XDG_CONFIG_HOME="$WORK/home/.config"

echo "== seeding metadata (local DB) =="
$XILO cache create default/nixpkgs
$XILO cache create default/ci --private --priority 20
$XILO cache configure default/ci --max-size 512MB --retention 720h
ADMIN_TOK=$($XILO token create demo-admin --admin | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
PUSH_TOK=$($XILO token create nixpkgs-push --cache default/nixpkgs --push --pull | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
CI_TOK=$($XILO token create ci-push --cache default/ci --push --pull --ttl 720h | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
DEAD=$($XILO token create old-laptop --cache default/nixpkgs --pull | grep -oE '[A-Za-z0-9_-]{40,}' | head -1)
DEAD_ID=$($XILO token list | grep old-laptop | grep -oE '^[0-9 ]+' | tr -d ' ' | head -1)
$XILO token revoke "$DEAD_ID"

echo "== starting server on $URL =="
$XILO serve &
SRV=$!
trap 'kill $SRV 2>/dev/null; wait $SRV 2>/dev/null; echo; echo "demo stopped; data kept in $WORK"' EXIT
for _ in $(seq 1 100); do curl -fs "$URL/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -fs "$URL/healthz" >/dev/null || { echo "server never came up"; exit 1; }

echo "== admin actions (fills the activities page) =="
JAR="$WORK/cookies"
post() { curl -fs -o /dev/null -b "$JAR" -c "$JAR" -H "Origin: $URL" "$URL$1" "${@:2}"; }
post /admin/login -d username=admin -d "password=$ADMIN_PW"
post /admin/users -d username=alice -d password=alicepass1 -d email=alice@example.com
post /admin/users -d username=bob -d password=bobpass123
post /admin/orgs -d name=acme
post /admin/plans -d name=free -d public=on -d max_caches=3 -d max_members=5
post /admin/caches -d namespace=acme -d name=web -d priority=30
post /admin/settings/instance -d allow_regs=on
# API traffic through the admin token shows up as token/CLI actor.
$XILO cache configure default/nixpkgs --priority 35 --server "$URL" --token "$ADMIN_TOK"
$XILO token create api-made --cache acme/web --pull --server "$URL" --token "$ADMIN_TOK" >/dev/null
curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $ADMIN_TOK" "$URL/api/v1/caches/acme/nope" || true

echo "== pushing a real closure =="
closure_root() {
	local p
	p=$(realpath "$(command -v bash)" 2>/dev/null | grep -oE '/nix/store/[^/]+')
	[ -n "$p" ] && { echo "$p"; return; }
	nix build nixpkgs#hello --no-link --print-out-paths 2>/dev/null | head -1
}
ROOT=$(closure_root)
if [ -n "$ROOT" ]; then
	$XILO login "$URL" --token "$PUSH_TOK" >/dev/null
	$XILO push default/nixpkgs "$ROOT" --quiet
	# Same closure into the private cache: exercises adoption + dedup stats.
	XILO_TOKEN=$CI_TOK $XILO push default/ci "$ROOT" --quiet
	# One path in ci-only so the two caches differ.
	if [ -e /nix/store ]; then
		EXTRA=$(realpath "$(command -v curl)" 2>/dev/null | grep -oE '/nix/store/[^/]+' || true)
		[ -n "$EXTRA" ] && XILO_TOKEN=$CI_TOK $XILO push default/ci "$EXTRA" --quiet || true
	fi
	# A few pulls so "last pulled" and hit-rate move.
	H=${ROOT#/nix/store/}; H=${H%%-*}
	for _ in 1 2 3; do curl -fs -o /dev/null "$URL/c/default/nixpkgs/$H.narinfo"; done
	curl -fs -o /dev/null "$URL/c/default/nixpkgs/nar/$H.nar"
else
	echo "no nix store path found; caches stay empty"
fi

cat <<MSG

  demo ready:  $URL/admin
  sign in:     admin / $ADMIN_PW   (also alice / alicepass1)
  path page:   $URL/admin/cache/default/nixpkgs
  activities:  $URL/admin/audit
  data dir:    $WORK   (Ctrl-C to stop)

MSG
# $BROWSER wins over the desktop default (xdg-open), e.g. BROWSER=firefox.
XDG_CONFIG_HOME="$REAL_XDG" "${BROWSER:-xdg-open}" "$URL/admin" >/dev/null 2>&1 || true
wait $SRV
