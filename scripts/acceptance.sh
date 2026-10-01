#!/usr/bin/env bash
# acceptance.sh: the local-track acceptance run (spec §11.2) on this
# device's real transcripts, read-only, in a scratch directory.
#
#   a. full-corpus index: wall, peak RSS, idle RSS after 60s, no-change
#      sweep CPU                      (flopwire bench acceptance --only index)
#   b. a line appended to a copied live session is findable locally
#                                     (--only fresh)
#   c. FAD 0.3.1 parity (and agentsview, with AGENTSVIEW_SRC) on a sample
#      of the real corpus             (scripts/oracle-sample.sh)
#   d. the harvested query set returns its expected hits, each under 200ms
#                                     (--only queries)
#
# Prints the pass/fail table, writes the run's record to
# <scratch>/acceptance-record.json and compares it with the most recent
# record under docs/perf/ (flopwire bench compare). A metric that grew by
# more than 20% is flagged REGRESSED. Records from a different machine or
# a much different corpus are not comparable; the comparison warns.
#
# Exit status: nonzero if a step fails. With --strict, also nonzero when
# any metric regressed by more than 20% or any check FAILs (the full index
# wall FAILs its 5 min bar today, so --strict fails until that is fixed).
#
# The whole run takes 10-15 minutes. FLOPWIRE_VERSION names the version in
# the record (default: git describe).
#
# Usage: [AGENTSVIEW_SRC=dir] [FLOPWIRE_VERSION=vX.Y.Z] scripts/acceptance.sh [--strict] <scratch dir>
set -euo pipefail

strict=()
if [[ "${1:-}" == "--strict" ]]; then
  strict=(--strict)
  shift
fi
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="${1:?usage: acceptance.sh [--strict] <scratch dir>}"
mkdir -p "$scratch"
cd "$root"
version="${FLOPWIRE_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
commit="$(git rev-parse HEAD)"
dirty=false
if [[ -n "$(git status --porcelain)" ]]; then dirty=true; fi
go build -ldflags="-X main.version=$version -X main.buildCommit=$commit -X main.buildDirty=$dirty" -o "$scratch/flopwire" ./cmd/flopwire
bench() { "$scratch/flopwire" bench acceptance --scratch "$scratch" "$@"; }
bench --only index
bench --only fresh
scripts/oracle-sample.sh "$scratch"
bench --only queries --queries "$root/testdata/acceptance/queries.yaml"
record="$scratch/acceptance-record.json"
bench --only report --json "$record"

echo
if compgen -G "$root/docs/perf/*.json" >/dev/null; then
  "$scratch/flopwire" bench compare ${strict[@]+"${strict[@]}"} "$root/docs/perf" "$record"
else
  echo "acceptance: no record under docs/perf/ to compare with"
  if [[ ${#strict[@]} -gt 0 ]]; then
    "$scratch/flopwire" bench compare --strict "$record" "$record"
  fi
fi
