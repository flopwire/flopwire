# Issue #136 compatibility smoke

The pin remains `9e4193da7c346ff14a377128920f3943ff14f523`.
The historical acceptance record and its provenance stay unchanged.
The commands below compare the actual pinned source with the current source
using the current benchmark implementation.

Run these commands from the source checkout containing the benchmark fix.
Schedule the builds and comparison separately from other heavy checks.
The commands require Go, jq and the sqlite3 CLI.
Use a new temporary directory for each comparison.

```sh
set -eu
smoke_dir="$(mktemp -d "${TMPDIR:-/tmp}/flopwire-136-smoke.XXXXXX")"
baseline_sha=9e4193da7c346ff14a377128920f3943ff14f523
candidate_label="$(git rev-parse HEAD)"
if ! git diff --quiet || ! git diff --cached --quiet; then
  candidate_label="$candidate_label-working-tree"
fi
mkdir "$smoke_dir/old-src"
git archive "$baseline_sha" | tar -x -C "$smoke_dir/old-src"
(cd "$smoke_dir/old-src" && go build -o "$smoke_dir/flopwire-old" ./cmd/flopwire)
go build -o "$smoke_dir/flopwire-current" ./cmd/flopwire
"$smoke_dir/flopwire-current" bench corpus --out "$smoke_dir/corpus" \
  --seed 1 --size 40MiB --big-min 3MiB --verify
"$smoke_dir/flopwire-current" bench ab \
  --a "$smoke_dir/flopwire-old" --a-commit "$baseline_sha" \
  --b "$smoke_dir/flopwire-current" --b-commit "$candidate_label" \
  --home "$smoke_dir/corpus" --scratch "$smoke_dir/scratch" \
  --out "$smoke_dir/results" --runs 1 --idle-after 1s \
  --only index,fresh,queries
cat "$smoke_dir/results/ab.md"
jq -e '.verdict != "BASELINE_FAILED" and (.error // "") == ""
  and (.checks | length > 0) and all(.checks[]; .a == "PASS" and .b == "PASS")
  and (.a_index_rows | length > 0) and all(.a_index_rows[]; . > 0)
  and (.b_index_rows | length > 0) and all(.b_index_rows[]; . > 0)' \
  "$smoke_dir/results/ab.json"
printf 'Smoke artifacts: %s\n' "$smoke_dir"
```

Retain both binaries and the generated corpus, per-run records, median records,
`ab.json` and `ab.md`. Record the candidate checkout and its diff if the label
ends in `working-tree`.

Check that A completes indexing, idle startup, freshness and queries without an
unknown `-opencode-db` error. A `REGRESSED` verdict reports measured drift;
it is separate from a compatibility failure on this one-run smoke. Check the query results and indexed row counts for
both binaries. Report other incompatibilities as failures.

On 2026-10-06, the pinned baseline and candidate `5f7ba257fc5a` completed
this 40 MiB comparison from a clean QA checkout. That candidate is a
QA cherry-pick with the identical Git tree as `91068e3`; only its commit
metadata differs. Both indexed 9,187 rows across 54 sources; all 12
query checks passed on each side, and the verdict was `CLEAN`.
Focused fake-binary tests also passed for capability selection and launch arguments. A successful smoke establishes
compatibility on this small corpus. One run and a one-second idle interval do
not establish performance confidence. Run the standard 1.5GB, three-run ABBA
comparison with the normal idle interval before evaluating performance drift.
