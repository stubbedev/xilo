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
# Nix runs this hook synchronously: without --detach the build waits for the
# upload, which is painful for a big closure (a dev shell, say). --detach hands
# the push to a background process and returns at once; progress and errors land
# in ~/.cache/xilo/push.log (the Nix daemon's cache dir when the daemon runs it).
# Drop --detach if you'd rather have the build fail when a push fails.
#
# Nix sets $OUT_PATHS to the space-separated built paths.
set -eu
[ -n "${OUT_PATHS:-}" ] || exit 0
printf '%s\n' $OUT_PATHS | xilo push "${XILO_CACHE:?set XILO_CACHE}" - --quiet --detach
