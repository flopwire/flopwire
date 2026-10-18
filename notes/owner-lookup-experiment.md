# Merged change attribution experiment

A merged SHA can differ from the commits recorded in session history.
This offline experiment follows a PR's original commits and returns evidence
candidates for inspection. It leaves owner selection and contact to the caller.

## Try it

Run from the repository root with Python 3. The fixture combines public
[PR #247 metadata](https://github.com/flopwire/flopwire/pull/247) with synthetic sessions.

```sh
python3 -m unittest discover -s scripts/owner-lookup-experiment -v
python3 scripts/owner-lookup-experiment/candidates.py \
  --pr-metadata scripts/owner-lookup-experiment/testdata/pr-247.json \
  --sessions scripts/owner-lookup-experiment/testdata/sessions.json \
  --target 33501273c320600cbcb2549387a4319ee7855c41
```

`--target` accepts the PR URL or its full merged SHA. Results distinguish
observed commits, PR mentions and ambiguous hash prefixes. Inspect the source
session before attributing the change: a PR mention can belong to a reviewer.

## Findings

A local probe on 2026-10-13 used a CLI built from `af87e78` and examined 480
sessions. Repository scope recovered candidates that the PR branch filter missed.

| PR | Merged SHA | Original PR commit | PR reference | Branch filter |
|---|---:|---:|---:|---:|
| [#245](https://github.com/flopwire/flopwire/pull/245) | 0 | 0 | 3 | 0 |
| [#247](https://github.com/flopwire/flopwire/pull/247) | 0 | 1 | 2 | 0 |
| [#239](https://github.com/flopwire/flopwire/pull/239) | 0 | 1 | 2 | 0 |

Cells count sessions; evidence types overlap. Eleven digests had capped commit
or PR lists. These counts describe accessible history, not verified authorship.
Qualification details and earlier observations are in [PR #252](https://github.com/flopwire/flopwire/pull/252).

<details>
<summary>Repeat the local probe</summary>

```sh
go build -o /tmp/flopwire-owner-probe ./cmd/flopwire
python3 scripts/owner-lookup-experiment/probe.py \
  --checkout /path/to/flopwire --repository flopwire/flopwire \
  --flopwire-bin /tmp/flopwire-owner-probe --pr 245 --pr 247 --pr 239
```

The probe reads local digests and GitHub metadata in memory and prints aggregate
counts. Its projection supplies the offline input format; see the synthetic
[sessions fixture](../scripts/owner-lookup-experiment/testdata/sessions.json).

</details>

<details>
<summary>Evidence limits</summary>

Output points into the input JSON, not transcript byte addresses. Use the
suggested session outline to recover source evidence. Repository scope and
provenance are trusted input. Hash matching checks this PR's lineage only.
Rebases, amendments and missing history can hide candidates; several sessions
can contribute to one change. The [digest parser](../internal/digest/commits.go)
records commit-producing output; `gh pr merge` alone does not establish authorship.

</details>

Before a production interface, choose a dedicated `owners` tool or a `change`
filter on `sessions`, decide how to expose PR-only references, and decide whether
GitHub lookup is optional. Handoffs remain deferred.
