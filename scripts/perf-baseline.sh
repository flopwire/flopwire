#!/usr/bin/env bash
# perf-baseline.sh: pick the baseline commit (binary A) of the nightly
# perf A/B run (.github/workflows/perf-nightly.yml). Prints
# `sha=<commit>` and `source=<why>` lines for $GITHUB_OUTPUT.
#
# The baseline is pinned: it moves only when someone moves it. A baseline
# that followed the nightlies would let drift below the threshold (say 5%
# a night against a 20% rule) accumulate without ever being reported.
#
# Order:
#   1. $INPUT_BASELINE, when set (a one-off manual run). A full or short
#      commit SHA, or a v* tag; anything else is refused.
#   2. The newer of:
#        - the latest release tag v* reachable from HEAD, and
#        - the commit in docs/perf/nightly-baseline (the pin).
#      The pin wins only when the tag is its ancestor, that is, when the
#      pin was moved after the release (an accepted regression). A new
#      release supersedes an older pin.
#
# A missing or unreachable pin with no tag is an error, never a silent
# fallback to a moving commit. Run from the repository root of a full
# clone (fetch-depth 0). $PERF_BASELINE_FILE overrides the pin's path
# (tests).
set -euo pipefail

pin_file="${PERF_BASELINE_FILE:-docs/perf/nightly-baseline}"

commit() { git rev-parse --verify --quiet "$1^{commit}"; }

if [[ -n "${INPUT_BASELINE:-}" ]]; then
  if [[ ! "$INPUT_BASELINE" =~ ^([0-9a-f]{7,40}|v[0-9][0-9A-Za-z.+-]*)$ ]]; then
    echo "baseline input must be a commit SHA or a v* tag" >&2
    exit 1
  fi
  sha="$(commit "$INPUT_BASELINE")" || { echo "baseline $INPUT_BASELINE is not a commit" >&2; exit 1; }
  echo "sha=$sha"
  echo "source=input $INPUT_BASELINE"
  exit 0
fi

pin="" pin_sha=""
if [[ -f "$pin_file" ]]; then
  pin="$(tr -d '[:space:]' <"$pin_file")"
  if [[ ! "$pin" =~ ^[0-9a-f]{40}$ ]]; then
    echo "$pin_file must hold one full commit SHA, found '$pin'" >&2
    exit 1
  fi
  pin_sha="$(commit "$pin")" || { echo "$pin_file names $pin, which is not a commit in this clone" >&2; exit 1; }
  git merge-base --is-ancestor "$pin_sha" HEAD || { echo "$pin_file names $pin, which is not an ancestor of HEAD" >&2; exit 1; }
fi

tag_sha=""
if tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null)"; then
  tag_sha="$(commit "$tag")"
fi

if [[ -n "$tag_sha" ]] && { [[ -z "$pin_sha" ]] || ! git merge-base --is-ancestor "$tag_sha" "$pin_sha"; }; then
  echo "sha=$tag_sha"
  echo "source=release $tag"
  exit 0
fi
if [[ -n "$pin_sha" ]]; then
  echo "sha=$pin_sha"
  echo "source=pinned in $pin_file"
  exit 0
fi
echo "no baseline: no v* tag and no $pin_file" >&2
exit 1
