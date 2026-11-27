#!/usr/bin/env bash
# Master merge rules check (DEC-146). CI's test job runs
# `bash scripts/merge-rules-check.sh`; anyone can run it by hand.
#
# Master requires pull requests, blocks force pushes, and requires test, web,
# and all three end-to-end shards. Reviewed PRs can merge behind master when
# their checks pass. For a migration PR, check the proposed merge's numbering.
#
# It reads GET /repos/<repo>/rules/branches/master, the rules GitHub enforces
# on master from every active ruleset. The endpoint is public for a public
# repository; GH_TOKEN (CI's github.token) is sent when set, for the rate
# limit. It cannot see bypass actors, which only an admin can read; review
# checks those (DEC-146). Any answer it cannot read is a failure.
#
#   merge-rules-check.sh                    fetch the live rules
#   merge-rules-check.sh --rules-file FILE  check a saved response (tests)
set -euo pipefail
export LC_ALL=C

usage() {
  echo "usage: merge-rules-check.sh [--rules-file FILE]" >&2
  exit 2
}

command -v jq >/dev/null || {
  echo "merge-rules-check: jq is required" >&2
  exit 1
}

rules_file=""
case "$#" in
  0) ;;
  2) [[ "$1" == "--rules-file" ]] || usage; rules_file="$2" ;;
  *) usage ;;
esac

if [[ -n "$rules_file" ]]; then
  rules=$(cat -- "$rules_file")
else
  repo="${GITHUB_REPOSITORY:-grierj/fold}"
  url="${GITHUB_API_URL:-https://api.github.com}/repos/$repo/rules/branches/master"
  headers=(-H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28")
  if [[ -n "${GH_TOKEN:-}" ]]; then
    headers+=(-H "Authorization: Bearer $GH_TOKEN")
  fi
  rules=$(curl -fsS --retry 3 --retry-all-errors --max-time 30 "${headers[@]}" "$url") || {
    echo "merge-rules-check: could not read $url" >&2
    exit 1
  }
fi

# One line per missing rule; nothing when every rule is in place. `-n` with
# `input` makes an empty answer an error instead of a silent pass.
problems=$(jq -rn '
  def required: ["test", "web", "e2e (1)", "e2e (2)", "e2e (3)"];
  input |
  if type != "array" then
    "the answer is not a list of rules"
  else
    (if any(.[]; .type == "pull_request") then empty
     else "no pull_request rule: a writer could push to master without a pull request" end),
    (if any(.[]; .type == "non_fast_forward") then empty
     else "no non_fast_forward rule: master history could be rewritten" end),
    ([.[] | select(.type == "required_status_checks")
          | .parameters.required_status_checks[]?.context] as $have
     | required[] | select(. as $c | $have | index($c) | not)
     | "required status check \(.) is not required on master")
  end
' <<<"$rules") || {
  echo "merge-rules-check: the rules answer is not JSON" >&2
  exit 1
}

if [[ -n "$problems" ]]; then
  while IFS= read -r p; do
    echo "merge-rules-check: $p" >&2
  done <<<"$problems"
  exit 1
fi
echo "merge-rules-check: master requires pull requests, blocks force pushes, and requires test, web, and all three e2e checks"
