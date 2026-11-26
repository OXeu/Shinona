#!/usr/bin/env bash
set -euo pipefail

: "${BENCHER_API_KEY:?Set the benchmark project key}"
api="${BENCHER_API_URL:-https://api.bencher.dev}/v0/projects/${BENCHER_PROJECT:-shinona}/measures"
auth=(-H "Authorization: Bearer $BENCHER_API_KEY" -H 'Content-Type: application/json')
request() {
  curl --silent --show-error --fail-with-body --max-time 30 "${auth[@]}" "$@"
}

# Fetch registered units once. A subsequent report can only publish registered
# custom measures with matching units. Concurrent jobs can create the same measure.
registered=$(request "$api?per_page=100")
definitions=$(python3 "$(dirname "$0")/benchmark-metrics.py" measures | jq -r 'to_entries[] | [.key, .value] | @tsv')
while IFS=$'\t' read -r slug units; do
  measure=$(jq -c --arg slug "$slug" '[.[] | select(.slug == $slug)] | first // empty' <<< "$registered")
  if [[ -z "$measure" ]]; then
    payload=$(jq -nc --arg slug "$slug" --arg units "$units" '{name: $slug, slug: $slug, units: $units}')
    if ! measure=$(request --data-binary "$payload" "$api"); then
      measure=$(request "$api/$slug")
    fi
  fi
  if ! jq -e --arg slug "$slug" --arg units "$units" '.slug == $slug and .units == $units' <<< "$measure" >/dev/null; then
    echo "Bencher measure $slug is missing or uses different units. Expected $units." >&2
    exit 1
  fi
done <<< "$definitions"
