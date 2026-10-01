#!/usr/bin/env bash
# perf-baseline.sh: pick the baseline commit (binary A) of the nightly
# perf A/B run (.github/workflows/perf-nightly.yml). Prints
# `sha=<commit>` and `source=<why>` lines for $GITHUB_OUTPUT.
#
# Order:
#   1. $INPUT_BASELINE, when set (a manual run).
#   2. The latest release tag v* reachable from HEAD: the nightly reports
#      drift since the last release.
#   3. The baseline the previous nightly on main recorded (artifact
#      perf-baseline). A clean nightly records its own commit; a regressed
#      one carries its baseline forward, so a regression keeps being
#      reported until it is fixed or the baseline is reset by hand.
#   4. HEAD~1 (first parent): the state before the last merge.
#
# Needs a full clone (fetch-depth 0). Step 3 needs gh with a token that can
# read actions artifacts; without one it is skipped.
set -euo pipefail

commit() { git rev-parse --verify --quiet "$1^{commit}"; }

if [[ -n "${INPUT_BASELINE:-}" ]]; then
  sha="$(commit "$INPUT_BASELINE")" || { echo "baseline $INPUT_BASELINE is not a commit" >&2; exit 1; }
  echo "sha=$sha"
  echo "source=input $INPUT_BASELINE"
  exit 0
fi

if tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null)"; then
  echo "sha=$(commit "$tag")"
  echo "source=release $tag"
  exit 0
fi

if [[ -n "${GITHUB_REPOSITORY:-}" ]] && command -v gh >/dev/null; then
  id="$(gh api "repos/$GITHUB_REPOSITORY/actions/artifacts?name=perf-baseline&per_page=30" \
    --jq '[.artifacts[] | select(.expired == false and .workflow_run.head_branch == "main")] | sort_by(.created_at) | last | .id // empty' 2>/dev/null || true)"
  if [[ -n "$id" ]]; then
    zip="$(mktemp)"
    if gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$id/zip" >"$zip" 2>/dev/null; then
      prev="$(unzip -p "$zip" baseline.sha 2>/dev/null | tr -d '[:space:]' || true)"
      if [[ -n "$prev" ]] && sha="$(commit "$prev")" && git merge-base --is-ancestor "$sha" HEAD; then
        echo "sha=$sha"
        echo "source=previous nightly (artifact $id)"
        exit 0
      fi
    fi
  fi
fi

echo "sha=$(commit HEAD~1)"
echo "source=HEAD~1 (no release tag and no previous nightly)"
