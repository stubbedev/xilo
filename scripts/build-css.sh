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
# Tailwind ignores an @source glob that matches nothing *without erroring*, so a
# wrong path here silently drops every templui class and produces a stylesheet
# that only looks broken in the browser (~31 KB instead of ~83 KB). Ask go for
# the directory and refuse to build unless it is really there.
# The download is best-effort (offline sandboxes already have the module, and
# it is what forces extraction into the cache); the -d check below is the gate.
go mod download github.com/templui/templui 2>/dev/null || true
tp=$(go list -mod=mod -m -f '{{.Dir}}' github.com/templui/templui 2>/dev/null || true)
[ -n "$tp" ] && [ -d "$tp/components" ] || {
	echo "build-css: templui module dir not found (got '${tp:-}')" >&2
	echo "build-css: run 'go mod download github.com/templui/templui' and retry" >&2
	exit 1
}

mkdir -p internal/server/views/css internal/server/static
printf '@source "%s/components/**/*.templ";\n@source "%s/components/**/*.go";\n@source "%s/components/**/*.js";\n' "$tp" "$tp" "$tp" \
	> internal/server/views/css/sources.generated.css

out=internal/server/static/xilo-tw.css
: "${TAILWINDCSS:=tailwindcss}"
# shellcheck disable=SC2086
$TAILWINDCSS -i internal/server/views/css/input.css -o "$out" --minify

# Tailwind exits 0 on a stylesheet that scanned nothing, so the only way to
# notice a source glob that stopped matching is the size of what came out: the
# real sheet is ~83 KB, the preflight-and-theme-only one is ~31 KB.
# ponytail: a byte floor, not a class inventory — raise it if the UI ever
# legitimately shrinks past it.
bytes=$(wc -c < "$out")
[ "$bytes" -ge 60000 ] || {
	echo "build-css: $out is only $bytes bytes — the utility classes were not scanned" >&2
	echo "build-css: check the @source globs in internal/server/views/css/*.css" >&2
	exit 1
}
