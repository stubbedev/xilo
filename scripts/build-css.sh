#!/usr/bin/env sh
# Build the admin Tailwind stylesheet (internal/server/static/xilo-tw.css) from
# the templ views + the templui component sources. The output is a generated
# artifact — NOT committed — so every build (local, CI, Docker, Nix) runs this
# before `go build` (the CSS is embedded via //go:embed static).
#
# Requires `tailwindcss` (v4) on PATH; override with $TAILWINDCSS (e.g. the just
# recipe passes `nix run nixpkgs#tailwindcss_4 --`).
set -eu

cd "$(CDPATH= cd "$(dirname "$0")/.." && pwd)"

# Resolve the templui module dir (its component .templ/.go hold the utility
# classes Tailwind must scan). Works from the module cache in every environment.
ver=$(go list -mod=mod -m github.com/templui/templui | awk '{print $2}')
tp="$(go env GOMODCACHE)/github.com/templui/templui@${ver}"

mkdir -p internal/server/views/css internal/server/static
printf '@source "%s/components/**/*.templ";\n@source "%s/components/**/*.go";\n' "$tp" "$tp" \
	> internal/server/views/css/sources.generated.css

: "${TAILWINDCSS:=tailwindcss}"
# shellcheck disable=SC2086
$TAILWINDCSS -i internal/server/views/css/input.css -o internal/server/static/xilo-tw.css --minify
