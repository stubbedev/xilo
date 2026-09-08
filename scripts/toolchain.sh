#!/usr/bin/env bash
# Keep every Go version pin in the repo equal to go.mod's `go` directive.
#
#   scripts/toolchain.sh check   # fail listing whatever drifted (CI, just check)
#   scripts/toolchain.sh sync    # rewrite them all from go.mod
#
# go.mod is the single source of truth. CI's Go comes from `go-version-file:
# go.mod` and so can never drift, but three pins are written by hand and can:
# the release image, the race-profile image, and the nix package's compiler.
#
# This exists because the race-profile pin stayed on golang:1.26 through the
# 1.27 bump. The official golang images set GOTOOLCHAIN=local, so the
# container did not fetch what it needed, it refused to build; and because
# nothing in CI ran that profile, the failure sat there unnoticed. A pin
# nobody exercises is a pin nobody maintains, so the check has to be
# mechanical rather than a line in a checklist.
set -euo pipefail
cd "$(dirname "$0")/.."

MODE=${1:-check}

# major.minor from `go 1.27.0`; the patch is deliberately dropped, since image
# tags and nixpkgs attributes are only ever that precise.
WANT=$(awk '/^go [0-9]/ {split($2, v, "."); print v[1] "." v[2]; exit}' go.mod)
if [ -z "$WANT" ]; then
  echo "toolchain: could not read the go directive from go.mod" >&2
  exit 1
fi
UNDER=${WANT/./_} # 1.27 -> 1_27, for nixpkgs' go_1_27

# One line per pin: file <TAB> a grep -E pattern that must match <TAB> a sed
# -E expression that rewrites any version to the wanted one. Adding a pin
# means adding a line, and then it is covered forever.
rules=$(
  cat <<EOF
Dockerfile	FROM --platform=\\\$BUILDPLATFORM golang:${WANT}-alpine	s|golang:[0-9]+\\.[0-9]+-alpine|golang:${WANT}-alpine|g
tests/k6/compose.yaml	image: golang:${WANT}\$	s|image: golang:[0-9]+\\.[0-9]+|image: golang:${WANT}|g
flake.nix	go = pkgs.go_${UNDER};	s|go = pkgs\\.go_[0-9]+_[0-9]+;|go = pkgs.go_${UNDER};|g
EOF
)

drift=0
changed=0
while IFS=$'\t' read -r file pattern fix; do
  [ -n "$file" ] || continue
  if [ ! -f "$file" ]; then
    echo "toolchain: $file is missing; the pin it held is now unchecked" >&2
    drift=$((drift + 1))
    continue
  fi
  if grep -Eq -- "$pattern" "$file"; then
    continue
  fi
  if [ "$MODE" = sync ]; then
    sed -i -E -- "$fix" "$file"
    if grep -Eq -- "$pattern" "$file"; then
      echo "synced $file to Go $WANT"
      changed=$((changed + 1))
    else
      echo "toolchain: could not rewrite the pin in $file; fix it by hand" >&2
      drift=$((drift + 1))
    fi
  else
    echo "DRIFT $file does not pin Go $WANT (go.mod says $WANT)" >&2
    grep -nE 'golang:[0-9]+\.[0-9]+|go_[0-9]+_[0-9]+' "$file" | sed 's/^/      /' >&2
    drift=$((drift + 1))
  fi
done <<<"$rules"

if [ "$drift" -gt 0 ]; then
  if [ "$MODE" != sync ]; then
    echo >&2
    echo "Run 'just sync-toolchain' and commit." >&2
  fi
  exit 1
fi

if [ "$MODE" = sync ]; then
  [ "$changed" = 0 ] && echo "every Go pin already matches go.mod ($WANT)"
else
  echo "every Go pin matches go.mod ($WANT)"
fi
