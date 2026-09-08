#!/usr/bin/env bash
# Compare a k6 summary against the committed baseline and fail on regression.
#
#   tests/k6/compare.sh <baseline.json> <summary.json> [label]
#
# The k6 thresholds in perf.js are absolute floors sized for the slowest
# runner we care about, so they catch a catastrophe and nothing else: they
# pass at 5.8ms and they pass at 190ms. This is the part that notices the
# difference. Every metric the baseline names is compared against the run, and
# one that moved the wrong way by more than its own tolerance fails the job.
#
# Each baseline value is the WORST the metric was observed to be across many
# CI runs of the same code, with a per-metric tolerance on top; the runner's
# own spread is ~1.7x and no amount of optimizing makes a shared 2-core VM
# deterministic (see tests/k6/baseline.sh for the measurements). The band is
# still an order of magnitude tighter than the absolute floors in perf.js.
#
# The anchor is a ratchet: `just re-baseline` will not write a worse number
# without ALLOW_REGRESSION=1, so a slow run has to be answered with a faster
# server rather than a wider band.
#
# Re-baseline deliberately with `just re-baseline` after a change meant to move
# the numbers, and say in the commit message why they moved.
set -uo pipefail

BASELINE=${1:?usage: compare.sh <baseline.json> <summary.json> [label]}
SUMMARY=${2:?usage: compare.sh <baseline.json> <summary.json> [label]}
LABEL=${3:-perf}

[ -f "$SUMMARY" ] || { echo "compare: no summary at $SUMMARY" >&2; exit 1; }
[ -f "$BASELINE" ] || {
  echo "compare: no baseline at $BASELINE (run 'just re-baseline')" >&2
  exit 1
}

fails=0
checked=0

# The walk is over the baseline, not the run: a metric that vanishes from the
# summary is a regression too (a scenario stopped reporting), while a metric a
# new run adds is not something an old baseline can judge.
while IFS=$'\t' read -r metric stat want tol dir; do
  checked=$((checked + 1))
  got=$(jq -r --arg m "$metric" --arg s "$stat" \
    '.metrics[$m] | (.[$s] // .value) | select(. != null) | tostring' "$SUMMARY")
  if [ -z "$got" ]; then
    echo "REGRESSION [$LABEL] $metric $stat: missing from the run (baseline $want)"
    fails=$((fails + 1))
    continue
  fi

  if [ "$dir" = lower ]; then
    read -r limit bad <<<"$(awk -v w="$want" -v t="$tol" -v g="$got" \
      'BEGIN{l = w * (1 + t); printf "%.6g %d", l, (g > l) ? 1 : 0}')"
    word="above"
  else
    read -r limit bad <<<"$(awk -v w="$want" -v t="$tol" -v g="$got" \
      'BEGIN{l = w * (1 - t); printf "%.6g %d", l, (g < l) ? 1 : 0}')"
    word="below"
  fi
  pct=$(awk -v g="$got" -v w="$want" \
    'BEGIN{if (w == 0) print "was 0"; else printf "%+.1f%%", (g - w) / w * 100}')

  if [ "$bad" = 1 ]; then
    echo "REGRESSION [$LABEL] $metric $stat: $got vs baseline $want ($pct, $word the limit $limit)"
    fails=$((fails + 1))
  else
    printf 'ok          [%s] %s %s: %s vs baseline %s (%s)\n' \
      "$LABEL" "$metric" "$stat" "$got" "$want" "$pct"
  fi
done < <(jq -r '
  .metrics | to_entries[]
  | .key as $m
  | (.value.tolerance // 0.6) as $tol
  | (.value.direction // "lower") as $dir
  | .value | to_entries[]
  | select(.key | test("^p\\(|^count$|^max$|^avg$|^med$|^value$"))
  | [$m, .key, (.value | tostring), ($tol | tostring), $dir] | @tsv' "$BASELINE")

if [ "$checked" = 0 ]; then
  echo "compare: baseline $BASELINE named no metrics" >&2
  exit 1
fi

echo "compare [$LABEL]: $checked metrics compared, $fails regressions"
[ "$fails" = 0 ]
