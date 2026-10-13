# Merged change attribution experiment

2026-10-13. Source baseline: `af87e78` (current main when this branch started).
This is an offline experiment, not a production owner resolver or messaging tool.
Handoff generation is deferred.

## What the repository examples showed

The main commit for [PR #247](https://github.com/flopwire/flopwire/pull/247)
is `33501273c320600cbcb2549387a4319ee7855c41`. It is not one of the PR's
three original commits. GitHub's commit-to-PR endpoint returned #247 for this
merged SHA. Following the PR's original commit list therefore supplies a
possible connection that a direct merged-SHA lookup misses.

The same probe compared [#245](https://github.com/flopwire/flopwire/pull/245)
and [#239](https://github.com/flopwire/flopwire/pull/239). #245 and #247
have squash commits; #239 has a merge commit. The difference between merged
and original hashes matters in both cases.

A binary built from the `af87e78` production source queried the local index.
The repo-wide detailed query returned 480 sessions, without pagination. Only
aggregate counts were retained. The repeatable probe produced:

| PR | Sessions recording merged SHA | Sessions recording an original PR commit | Sessions referencing the PR | Sessions returned by current PR branch filter |
|---|---:|---:|---:|---:|
| #245 | 0 | 0 | 3 | 0 |
| #247 | 0 | 1 | 2 | 0 |
| #239 | 0 | 1 | 2 | 0 |

Counts overlap: a session can record both a commit and a PR reference. These are
visible digest counts, not verified authors or a complete history audit. Branch
filtering alone missed all candidates in this sample; use repository scope as
the starting point. A PR's current head branch is not an ownership identifier.
The initial inspection used installed CLI revision `04124b3` and found two
PR-reference sessions for #245 on both local and remote retrieval; the subsequent local reproduction found three as history changed. Neither
run found commit evidence for #245. Eleven of the 480 digests reported capped
commit or PR lists in the reproduction. The checked-in repeatable probe uses
local retrieval only. The current-main binary reproduced the later local
counts, including zero branch-filter results.

## Evidence the experiment preserves

`scripts/owner-lookup-experiment/candidates.py` joins a merged PR's metadata to
a deliberately projected session export. It returns every relevant session with
typed evidence and an outline-read suggestion. It does not pick an owner,
check presence, send a message, or introduce a CLI/MCP production command.

- `observed_pr_commit`: the digest's observed hash matches an original PR commit.
- `observed_merged_commit`: the digest records the final merged hash. This does
  not establish authorship of all constituent changes.
- `pr_reference`: the digest mentions the exact repository and PR number.
  A reviewer or merge session can mention the same PR as its producing session.
- `ambiguous_commit_prefix`: the abbreviated hash matches multiple known
  commits. The experiment preserves the ambiguity.

Output includes the merged SHA, original commit list, observed digest values,
and JSON field positions. These positions point into the supplied export;
they are not transcript byte addresses. Recover message-level provenance with
`flopwire_read address=SESSION outline=true` and bounded transcript reads before
contacting a session. The outline suggestion is not executed by the experiment.

The fixture uses public #247 GitHub metadata and entirely synthetic sessions.
It demonstrates both a commit-recording session and a PR-only reference session;
it does not identify a real user session as the author.

## Run the offline demonstration

Run from the repository root. Python 3's standard library is sufficient.

```sh
python3 -m unittest discover -s scripts/owner-lookup-experiment -v
python3 scripts/owner-lookup-experiment/candidates.py \
  --pr-metadata scripts/owner-lookup-experiment/testdata/pr-247.json \
  --sessions scripts/owner-lookup-experiment/testdata/sessions.json \
  --target 33501273c320600cbcb2549387a4319ee7855c41
```

Use the canonical PR URL as `--target` to start from the PR instead. The input
metadata must be for that PR or the exact merged SHA. This deliberately avoids
selecting one PR automatically when commit association returns several.

Obtain GitHub metadata with these read-only commands:

```sh
gh api repos/flopwire/flopwire/commits/33501273c320600cbcb2549387a4319ee7855c41/pulls \
  --jq 'map({number,merged_at,base_repo:.base.repo.full_name})'
gh pr view 247 --repo flopwire/flopwire \
  --json number,url,mergeCommit,commits,headRefName,state
```

For an optional local-history reproduction, run:

```sh
python3 scripts/owner-lookup-experiment/probe.py \
  --checkout /path/to/flopwire --repository flopwire/flopwire \
  --flopwire-bin /path/to/current/flopwire --pr 245 --pr 247 --pr 239
```

Build the current CLI with `go build -o /tmp/flopwire-owner-probe ./cmd/flopwire`
and pass that binary to avoid testing an older installed version.

This reads local session digests and GitHub PR metadata in memory. It prints
aggregate counts only, not session IDs, titles, prompts, transcript bodies or
tool errors. It writes no history export. Counts depend on the device's
accessible index and can change after more sessions are indexed.

For custom offline input, provide `repository`, an explicit boolean `complete`,
and `sessions`. Each row supplies `repository`, `session_id`, observed `commits`,
exact `prs` references, and digest `more` truncation markers. This is a projected
experiment schema, not the native `flopwire_sessions` response. The probe shows
the projection from that response. Repository scope and provenance are trusted
input assertions, not authenticated by this reducer. Keep private exports out
of the repository.

## Limits and verification

The digest parser recognizes commit-producing tool output and later HEAD
revelations after quiet commits. It deliberately treats `gh pr merge` as a
HEAD move, not evidence that the caller authored the squash commit. See
[`internal/digest/commits.go`](../internal/digest/commits.go).
PR references can come from ordinary text; they do not prove PR creation.
Digests keep at most 20 commits and 10 PRs and expose caps through `more`.
See [`internal/digest/digest.go`](../internal/digest/digest.go).

Original commit IDs in GitHub can differ from pre-rebase or amended hashes
recorded in a session. A shared cherry-picked commit can have several relevant
sessions. Missing or capped digest evidence, denied transcripts, unavailable
devices and older histories all prevent a negative ownership conclusion.
The experiment matches hash prefixes only against this PR's known lineage;
it does not prove global uniqueness of an abbreviated hash. The production
design would need stronger repository-wide collision handling.

Eight offline tests cover squash lineage, multiple candidates, prefix
collisions, merged-SHA evidence, missing/capped exports, repository and target
validation, duplicate sessions, and invalid short hashes. The real-history
probe is a spot check, not an ownership precision/recall benchmark. No runtime
Go, server, database or plugin code changes in this experiment.

## Design choices before implementation

The proposed first production interface is a read-only result for a merged SHA
or repository-qualified PR. It should return evidence candidates, ambiguity,
scan completeness and source addresses. Keep presence and contact as subsequent
explicit steps. Two decisions still need a product choice:

1. **Entry point:** a dedicated `flopwire owners TARGET` / `flopwire_owners`
   tool, or a `change` filter on `sessions`. A dedicated result can describe PR
   lineage and uncertainty; the filter fits the existing session workflow.
2. **Weak evidence:** show PR-only references in a separate candidate group,
   or return only commit-evidenced candidates and offer reference fallback on
   request. Either way, do not silently promote a mention to owner.

Before rollout, evaluate rebased and amended PRs, cherry-picks, multiple
contributing agents, unrelated PR mentions, capped digests and missing history.
Decide whether GitHub credentials and API access are optional enrichment or a
required dependency. Preserve a manual path when association cannot be resolved.
