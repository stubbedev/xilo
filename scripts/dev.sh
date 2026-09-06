#!/usr/bin/env bash
# `just dev`: the admin UI against a throwaway instance full of dummy data,
# served through air so every .go/.templ/.css save rebuilds and restarts the
# server; the page then patches itself in place (XILO_DEV morph reload, see
# devReloadScript in layout.templ) instead of doing a full reload.
# Everything lives under ./tmp/dev (git-ignored) and is wiped on every run.
# Seeds: two caches (one private + capped), tokens, an org, users, a plan, a
# real store-path closure pushed into each cache, and enough admin/API calls
# to fill the activities page. Ctrl-C stops everything.
#
#   just dev             # http://localhost:8090, admin / demo
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=8090
URL="http://localhost:$PORT"
APP="$URL"
WORK="$PWD/tmp/dev"
XILO=./tmp/xilo
ADMIN_PW=demo

# Never wipe tmp/dev under a running instance: a second `just dev` would pull
# the database out from under the first and leave it hanging on busy retries.
if curl -fs -o /dev/null "$APP/healthz" 2>/dev/null; then
	echo "something already serves $URL (another just dev?); stop it first" >&2
	exit 1
fi

echo "== building =="
templ generate >/dev/null
just css >/dev/null
go build -o "$XILO" ./cmd/xilo

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

echo "== starting air on $URL (rebuild + in-place reload on save) =="
export XILO_DEV=1
air &
AIR=$!
trap 'kill $AIR 2>/dev/null; wait $AIR 2>/dev/null; echo; echo "dev server stopped"' EXIT
for _ in $(seq 1 300); do curl -fs "$APP/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -fs "$APP/healthz" >/dev/null || { echo "server never came up"; exit 1; }

echo "== admin actions (fills the activities page) =="
JAR="$WORK/cookies"
post() { curl -fs -o /dev/null -b "$JAR" -c "$JAR" -H "Origin: $APP" "$APP$1" "${@:2}"; }
post /admin/login -d username=admin -d "password=$ADMIN_PW"
post /admin/users -d username=alice -d password=alicepass1 -d email=alice@example.com
post /admin/users -d username=bob -d password=bobpass123
post /admin/orgs -d name=acme
post /admin/plans -d name=free -d public=on -d max_caches=3 -d max_members=5
post /admin/caches -d namespace=acme -d name=web -d priority=30
post /admin/settings/instance -d allow_regs=on
# API traffic through the admin token shows up as token/CLI actor.
$XILO cache configure default/nixpkgs --priority 35 --server "$APP" --token "$ADMIN_TOK"
$XILO token create api-made --cache acme/web --pull --server "$APP" --token "$ADMIN_TOK" >/dev/null
curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $ADMIN_TOK" "$APP/api/v1/caches/acme/nope" || true

echo "== pushing a real closure =="
closure_root() {
	local p
	p=$(realpath "$(command -v bash)" 2>/dev/null | grep -oE '/nix/store/[^/]+')
	[ -n "$p" ] && { echo "$p"; return; }
	nix build nixpkgs#hello --no-link --print-out-paths 2>/dev/null | head -1
}
ROOT=$(closure_root)
if [ -n "$ROOT" ]; then
	$XILO login "$APP" --token "$PUSH_TOK" >/dev/null
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
	for _ in 1 2 3; do curl -fs -o /dev/null "$APP/c/default/nixpkgs/$H.narinfo"; done
	curl -fs -o /dev/null "$APP/c/default/nixpkgs/nar/$H.nar"
else
	echo "no nix store path found; caches stay empty"
fi

cat <<MSG

  dev ready:   $URL/admin
  sign in:     admin / $ADMIN_PW   (also alice / alicepass1)
  path page:   $URL/admin/cache/default/nixpkgs
  activities:  $URL/admin/audit
  data dir:    $WORK   (Ctrl-C stops; saving .go/.templ/.css rebuilds and patches the page)

MSG
# $BROWSER wins over the desktop default (xdg-open), e.g. BROWSER=firefox.
XDG_CONFIG_HOME="$REAL_XDG" "${BROWSER:-xdg-open}" "$URL/admin" >/dev/null 2>&1 || true
wait $AIR
