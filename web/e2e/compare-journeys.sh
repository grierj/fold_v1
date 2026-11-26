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
head_revision=$(git -C "$root" rev-parse --verify HEAD) || exit 1
base_revision=$(git -C "$root" rev-parse --verify "$head_revision^") || base_revision=

for side in base head; do
  (
    set -e
    source_dir="$root"
    revision=$head_revision
    if [[ $side == base ]]; then
      revision=$base_revision
      [[ -n "$revision" ]]
      source_dir="$work/base"
      mkdir "$source_dir"
      git -C "$root" archive "$revision" | tar -x -C "$source_dir"
      (cd "$source_dir/web" && npm ci)
    fi
    (cd "$source_dir/web" && npm run build)
    export FOLD_E2E_BIN="$work/fold-$side"
    (cd "$source_dir" && go build -trimpath -o "$FOLD_E2E_BIN" .)
    export JOURNEY_REVISION="$revision" JOURNEY_DIST="$source_dir/web/dist"
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

node "$root/web/scripts/journey-report.mjs" "$results/base/samples.json" "$results/head/samples.json" \
  "$base_revision" "$head_revision" >> "$report" || failed=1
cat "$report" >> "${GITHUB_STEP_SUMMARY:-/dev/stdout}" || failed=1
exit "$failed"
