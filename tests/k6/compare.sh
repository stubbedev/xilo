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
# Tolerances are per metric and wide on purpose: a shared CI runner varies a
# lot between runs, and a gate that cries wolf gets deleted. Even at 100% they
# are an order of magnitude tighter than "p(95) < 5000".
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

# Metrics where a rising number is the bad direction. Anything else the
# baseline names (throughput, counts of work done) is higher-is-better.
LOWER_IS_BETTER='^(http_req_duration|iteration_duration|http_req_waiting|http_req_blocked|http_req_receiving|srv_heap_mib|srv_goroutines|dropped_iterations|http_req_failed)'

fails=0
checked=0

# The walk is over the baseline, not the run: a metric that vanishes from the
# summary is a regression too (a scenario stopped reporting), while a metric a
# new run adds is not something an old baseline can judge.
while IFS=$'\t' read -r metric stat want tol; do
  checked=$((checked + 1))
  got=$(jq -r --arg m "$metric" --arg s "$stat" \
    '.metrics[$m] | (.[$s] // .value) | select(. != null) | tostring' "$SUMMARY")
  if [ -z "$got" ]; then
    echo "REGRESSION [$LABEL] $metric $stat: missing from the run (baseline $want)"
    fails=$((fails + 1))
    continue
  fi

  if [[ $metric =~ $LOWER_IS_BETTER ]]; then
    dir=worse
    read -r limit bad <<<"$(awk -v w="$want" -v t="$tol" -v g="$got" \
      'BEGIN{l = w * (1 + t); printf "%.6g %d", l, (g > l) ? 1 : 0}')"
  else
    dir=lower
    read -r limit bad <<<"$(awk -v w="$want" -v t="$tol" -v g="$got" \
      'BEGIN{l = w * (1 - t); printf "%.6g %d", l, (g < l) ? 1 : 0}')"
  fi
  pct=$(awk -v g="$got" -v w="$want" \
    'BEGIN{if (w == 0) print "was 0"; else printf "%+.1f%%", (g - w) / w * 100}')

  if [ "$bad" = 1 ]; then
    echo "REGRESSION [$LABEL] $metric $stat: $got vs baseline $want ($pct, $dir than the limit $limit)"
    fails=$((fails + 1))
  else
    printf 'ok          [%s] %s %s: %s vs baseline %s (%s)\n' \
      "$LABEL" "$metric" "$stat" "$got" "$want" "$pct"
  fi
done < <(jq -r '
  (.tolerance // 0.5) as $default
  | .metrics | to_entries[]
  | .key as $m
  | (.value.tolerance // $default) as $tol
  | .value | to_entries[]
  | select(.key != "tolerance")
  | [$m, .key, (.value | tostring), ($tol | tostring)] | @tsv' "$BASELINE")

if [ "$checked" = 0 ]; then
  echo "compare: baseline $BASELINE named no metrics" >&2
  exit 1
fi

echo "compare [$LABEL]: $checked metrics compared, $fails regressions"
[ "$fails" = 0 ]
