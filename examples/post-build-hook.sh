#!/bin/sh
# Nix post-build-hook: push every freshly-built path to xilo automatically.
#
# Install:
#   1. Put this script somewhere executable, e.g. /etc/nix/xilo-post-build-hook.sh
#   2. In nix.conf / nix.conf.d:
#        post-build-hook = /etc/nix/xilo-post-build-hook.sh
#   3. Export the server + token for the Nix daemon (in the systemd unit or env):
#        XILO_URL=https://cache.example.com
#        XILO_TOKEN=<a push token>
#        XILO_CACHE=mycache
#
# Nix sets $OUT_PATHS to the space-separated built paths.
set -eu
[ -n "${OUT_PATHS:-}" ] || exit 0
printf '%s\n' $OUT_PATHS | xilo push "${XILO_CACHE:?set XILO_CACHE}" - --quiet
