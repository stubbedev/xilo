#!/usr/bin/env bash
# Run every fuzz target in the repo for a given duration.
#
#   scripts/fuzz.sh 30s          # what CI does on each push
#   scripts/fuzz.sh 4m           # what the nightly does
#   scripts/fuzz.sh 30s ./internal/chunk   # one package
#
# The target list is DISCOVERED, not maintained. It used to be written out in
# both ci.yml and nightly.yml, so adding a target meant remembering two files
# neither of which would complain: the new target would simply never run,
# which is the same failure as the NAR differential tests skipping in CI. A
# list nobody is forced to update is a list that goes stale.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

FUZZTIME=${1:?usage: fuzz.sh <fuzztime> [package...]}
shift
PKGS=("$@")
[ "${#PKGS[@]}" -gt 0 ] || PKGS=(./internal/...)

# One "<package> <FuzzName>" per line, straight from the source.
targets=$(
  go list -f '{{.Dir}} {{.ImportPath}}' "${PKGS[@]}" 2>/dev/null | while read -r dir importpath; do
    grep -hoE '^func (Fuzz[A-Za-z0-9_]*)\(' "$dir"/*_test.go 2>/dev/null |
      sed -E 's/^func (Fuzz[A-Za-z0-9_]*)\($/\1/' | while read -r name; do
      [ -n "$name" ] && echo "$importpath $name"
    done
  done | sort -u
)

if [ -z "$targets" ]; then
  echo "fuzz: no Fuzz targets found under ${PKGS[*]}" >&2
  exit 1
fi

count=$(echo "$targets" | wc -l | tr -d ' ')
echo "fuzzing $count target(s) for $FUZZTIME each"

fail=0
while read -r pkg target; do
  [ -n "$pkg" ] || continue
  # ::group:: folds the run on GitHub and is harmless in a terminal.
  echo "::group::$target ($pkg)"
  if ! go test "$pkg" -run "^$target\$" -fuzz "^$target\$" -fuzztime="$FUZZTIME" -count=1; then
    fail=1
    echo "FAILED $target ($pkg)"
  fi
  echo "::endgroup::"
done <<<"$targets"

if [ "$fail" != 0 ]; then
  echo >&2
  echo "A crasher is written to that package's testdata/fuzz: commit it, since" >&2
  echo "the corpus entry is the reproduction." >&2
fi
exit "$fail"
