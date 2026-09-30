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
# Prints the pass/fail table at the end. The whole run takes 10-15 minutes.
#
# Usage: [AGENTSVIEW_SRC=dir] scripts/acceptance.sh <scratch dir>
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="${1:?usage: acceptance.sh <scratch dir>}"
mkdir -p "$scratch"
cd "$root"
go build -o "$scratch/flopwire" ./cmd/flopwire
bench() { "$scratch/flopwire" bench acceptance --scratch "$scratch" "$@"; }
bench --only index
bench --only fresh
scripts/oracle-sample.sh "$scratch"
bench --only queries --queries "$root/testdata/acceptance/queries.yaml"
bench --only report
