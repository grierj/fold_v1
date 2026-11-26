#!/usr/bin/env bash
# Continue after a build or measurement failure so the other revision and report still run.
set +e
set -uo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd) || exit 1
work=${RUNNER_TEMP:?}
mkdir -p "$root/web/test-results" || exit 1
results=$(mktemp -d "$root/web/test-results/comparison.XXXXXX") || exit 1
report="$results/report.md"
export CGO_ENABLED=0
failed=0

for side in base head; do
  (
    set -e
    source_dir="$root"
    ref=HEAD
    if [[ $side == base ]]; then
      source_dir="$work/base"
      ref=HEAD^
      mkdir "$source_dir"
      git -C "$root" archive "$ref" | tar -x -C "$source_dir"
      (cd "$source_dir/web" && npm ci)
    fi
    (cd "$source_dir/web" && npm run build)
    export FOLD_E2E_BIN="$work/fold-$side"
    (cd "$source_dir" && go build -trimpath -o "$FOLD_E2E_BIN" .)
    JOURNEY_REVISION=$(git -C "$root" rev-parse "$ref")
    export JOURNEY_REVISION JOURNEY_DIST="$source_dir/web/dist"
    cd "$root/web"
    E2E_PORT="${E2E_PORT:-18419}" E2E_SHARD="journey_$side" E2E_OUTPUT_DIR="$results/$side" npm run journeys
  )
  status=$?
  if ((status)); then
    echo "- $side build or measurement failed (exit $status)." >> "$report"
    failed=1
  else
    echo "- $side measurement completed." >> "$report"
  fi
done

node "$root/web/scripts/journey-report.mjs" "$results/base/samples.json" "$results/head/samples.json" >> "$report" || failed=1
cat "$report" >> "${GITHUB_STEP_SUMMARY:-/dev/stdout}" || failed=1
exit "$failed"
