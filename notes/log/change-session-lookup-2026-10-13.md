# Find the sessions behind a change

Record of 2026-10-13. Editor's note, 2026-10-18: the lookup it designs shipped
as `flopwire trace` (MCP `flopwire_change`); see [docs/search.md](../../docs/search.md).

**Proposal:** look up a commit ID and get matching sessions, with links to the
captured commands and results. Several sessions can match; the evidence explains
why each one appeared.

## How it would work

1. The agent asks Flopwire about a commit.
2. Flopwire returns matching sessions and links the agent can open with `read`.
3. If the evidence is insufficient, the agent uses its existing `gh` access to
   find the PR's original commits, then looks those up together.

That third step matters for squash merges: the merged commit has a different ID
from the commits recorded in the original sessions. **GitHub credentials stay
with the calling agent.** The Flopwire daemon receives only commit IDs.

## What to build

Add a commit index derived from the sessions Flopwire already captures.
Keep the command and result addresses so each match can be inspected. Existing
session summaries are capped and discard those addresses.

Entire's explicit commit-to-context links are useful. For Flopwire, an internal
index reuses existing capture and storage without requiring Git hooks or changes
to commit messages. The main implementation cost is keeping that index correct
when source records are reparsed, redacted, or deleted.

An empty result means no evidence was found in the searched history. Missing
captures, rewritten commits, and abbreviated hashes can leave gaps or ambiguity.

**Status:** design only. The state walk and Git experiment are complete;
implementation and performance checks remain. Build the event extractor first,
then the local/server indexes, then the lookup tool.

<details>
<summary>Proposed tool and result format</summary>

`flopwire_change(repo, commits, limit?, cursor?)`

Use one dedicated tool so the task has one result shape. It searches the caller's
own sessions and descendants as well as other visible sessions.

| Input or output | Meaning |
|---|---|
| `repo` | Resolved repository scope |
| `commits` | Full commit IDs to look up; initially at most 100 per request |
| `limit`, `cursor` | Page through evidence events; initially 20 per page |
| Each match | Session, matched input ID, observed hash, operation, and command/result read addresses |
| Coverage | Partial pages, indexing lag, unavailable sources, or unknown command repository |

Group events by session for display. A session can recur on later pages. Bound
snippets and total output bytes; measure before finalizing the limits.

An operation is `commit`, `cherry_pick`, `revert`, or `unknown`. Compound commands
can produce several commits without showing which operation produced each one.
A quiet commit resolved by a later HEAD command links both events.

An abbreviated hash remains a prefix match, even if only one submitted ID matches
it. The input list cannot establish uniqueness across the whole repository.
PR mentions remain searchable through grep; this tool returns commit evidence.

The calling agent can start from a PR or investigate an associated PR after an
insufficient direct lookup. It keeps the GitHub response as evidence of the PR's
commit list. Flopwire treats supplied IDs as search inputs and makes no claim
about their PR relationship. The daemon and server do not invoke `gh`.

</details>

<details>
<summary>Implementation and checks needed before shipping</summary>

### 1. Preserve evidence in the existing parser

Extend the shared commit parser to emit events before the session summary drops
source details or hits its cap. Preserve addresses for the command, result, and
any later command that reveals the commit ID. Keep existing summary behavior.

Assign an operation only when command/result association supports it. Wrapper
scripts, absent output, and unsupported shell syntax remain gaps. Test quiet
commits, failed compound commands, directory changes, cherry-picks, and reverts.

Pair commands and results through parsed IDs within the same source/session.
Text containing an ID or a nearby URL is insufficient. Add fixtures for delayed
results returned by a later poll and self-contained command-completion records.
Preserve execution outcome separately: a failed compound command can still
contain evidence of an earlier successful commit. Settle this event contract
before designing the index schema.

Keep the command's observed directory separate from the session's repository.
A session can commit in another repository. Label the command repository known
only when captured placement evidence supports it; today's path or remote is
insufficient. Initial repository filtering uses session placement and can miss
cross-repository work.

### 2. Add rebuildable local and server indexes

Both indexes use the same extractor. Key events by source identity, generation,
and message identity. Index observed hashes and visible conversation identity.
Validate query plans and prefix matching at scale.

Publish events and their source generation atomically. Reparse replaces a
source generation's events without duplicates. Redaction, replacement, deletion,
and access changes must remove or hide the corresponding evidence. Test those
paths, backfill, and interrupted ingestion on both local and server storage.
The index can be rebuilt from retained source records.

### 3. Add bounded lookup and pagination

Page in stable event order with an ingestion cutoff. New appends appear in a new
lookup without restarting existing pages. Replacing or removing evidence
invalidates affected pages and returns a restart condition. Bind cursors to the
query and visibility scope; recheck access on each page. Keep raw evidence out
of cursor tokens.

The cursor contract still needs an implementation spike. Measure latency and
output size before fixing limits. Report visible-index coverage, indexing lag,
and truncation separately: a finished scan does not establish complete history.

</details>

<details>
<summary>What Entire gets right, and why use a different implementation</summary>

Entire v0.11.4 supports several sessions per checkpoint and several checkpoint
links per commit. Its local squash hooks preserve existing links, and rewrite
hooks handle amend/rebase mappings. Flopwire should retain that ability to
represent several contributors. [Sessions][entire-persistent],
[squash tests][entire-squash], [rewrite hooks][entire-hooks].

Those links have separate availability limits. Commit hooks can fail while the
commit succeeds. Checkpoint uploads can fail while code pushes succeed. Later
transcript backfills can update checkpoint refs. A link alone does not establish
complete, readable context. [Hook generation][entire-install],
[push handling][entire-push], [refs storage][entire-refs].

GitHub allows squash messages containing only the PR title or description.
Inference: local hooks cannot guarantee checkpoint trailers survive a hosted
merge. Looking up the PR's original commits remains useful.
[GitHub squash settings][github-squash].

Flopwire already stores captured sessions. Deriving the links there avoids adding
Entire's Git hooks, checkpoint refs, and separate publication queue to this feature.

</details>

<details>
<summary>Lessons from Agentboard's GitHub parsing</summary>

Reviewed Agentboard `ab34776d` locally on 2026-10-13.
Its [PR extractor][agentboard-extractor] recognizes command invocations, handles
shell wrappers, and uses call/result pairing or self-contained Codex command
records. Claude's `pr-link` records supply another session-to-PR association.
These are useful fixture patterns for Flopwire's existing parser.

All 54 extractor tests passed. Two additional synthetic checks showed why its
results should remain associations: an unrelated result containing a pending
call ID in its text was accepted; a failed create containing an existing PR URL
was accepted too. The fallback also accepts result URLs within ten lines of an
unrecognized create record. These signals cannot establish commit production.

Borrow the fixtures and exact event matching approach. Keep weak associations
out of commit results. Explicit `pr-link` support can be added separately as PR
association evidence; it is unnecessary for the first commit lookup.

Agentboard's separate [metadata fetcher][agentboard-info] invokes `gh` from its
server. Keep that credential choice separate: this proposal leaves `gh` with
the calling agent.

</details>

<details>
<summary>Hand-computed state transitions</summary>

This walk follows a squash merge through capture and lookup. All names and
commit symbols are synthetic.

```text
t0 git={main:Z}; capture={}; index={}
   caller={target:none}; provider={}; result={}

t1 session A runs git commit; output names C1
   git={topic:C1,parent:Z}; capture={A:[call1,result1]}
   index={A.digest.commits:[prefix(C1)]}; caller/provider/result unchanged
   Missing: digest has no result address, operation, or command directory.

t2 session B cherry-picks another change into C2 on topic
   git={topic:C2,parent:C1}; capture adds B:[call2,result2]
   index adds B.digest.commits:[prefix(C2)]; caller/provider/result unchanged
   Missing: digest cannot distinguish this backport from git commit.

t3 session M merges PR using hosted squash, producing S
   git={main:S,parent:Z,topic:C2}; capture adds M:[gh-pr-merge-call/result]
   index={A:[prefix(C1)],B:[prefix(C2)],M.prs:[PR]}
   caller={target:S}; provider={PR:{merge:S,commits:[C1,C2]}}; result={}
   Existing parser treats gh pr merge as a HEAD move, not commit creation.

t4 current direct lookup compares S with the digest hashes
   result={matches:[]}; all other state unchanged
   Empty does not establish that captured contributors are absent.

t5 caller asks GitHub for original commits, then searches [C1,C2]
   caller={target:S,providerEvidence:PR,lookup:[C1,C2]}
   result={candidates:[A,B]}; git/capture/index/provider unchanged
   Missing: event evidence and its availability must be recovered.

t6 proposed projection from the same capture
   index.events={A:{hash:C1-prefix,op:commit,call:call1,result:result1},
                 B:{hash:C2-prefix,op:cherry_pick,call:call2,result:result2}}
   result={A:prefix-match-C1,B:prefix-match-C2,coverage:visible-index}
   Caller retains PR→[C1,C2] relation. M is not promoted by its PR mention.
   No observation identifies a sole author or current contact availability.
```

Adjacent transitions, starting from t6:

| Transition | Explicit changed state | Required result |
|---|---|---|
| Rebase C1 to C1′ before GitHub records the final list | `provider.commits=[C1′,C2′]`; capture still names old IDs | No invented match; old/new relation remains unavailable |
| Commit result arrives after a lookup | `capture` gains result; next projection generation gains event | Earlier result reports current indexed scope; later lookup may differ |
| Source result is redacted between pages | `capture.result1=hidden`; event becomes inaccessible; generation invalidated | Page restart; no cached excerpt of result1 |
| Repo R session commits in repository Q | `event.commandDirectory=Q`; `session.repo=R` | Preserve command location; never assert R merely from session placement |
| Two inputs share a recorded short prefix | `lookup=[C1,C3]`; `prefix(C1)=prefix(C3)` | One ambiguous observation with both possible matches |
| Another matching commit is outside the submitted list | `lookup=[C1]`; repository also has C3 with same prefix | Still report a prefix match, without a uniqueness claim |
| Indexed source is complete but another device is offline | `index.scan=complete`; `capture.remoteCoverage=unknown` | Complete visible-index scan, unknown overall history |

Invented state required by this walk: event addresses and operation, captured
command placement, projection generation, backfill/coverage state, and explicit
invalidation. PR lineage stays in the caller's context, outside daemon state.

</details>

<details>
<summary>Research findings and Git experiment</summary>

Reviewed on 2026-10-13 against Flopwire `208f48b` and Entire v0.11.4. The
[earlier experiment](https://github.com/flopwire/flopwire/pull/252) recovered
candidates by following original PR commits after squash merges.

| Finding | Evidence and consequence |
|---|---|
| Session summaries do not establish the command's repository | [Parser tests](https://github.com/flopwire/flopwire/blob/208f48b33f22d73f5d971e8b6859b4379158ada7/internal/digest/commits_test.go) count outside-directory commits. Preserve location and uncertainty. |
| Summaries can be incomplete or stale | [Digest caps](https://github.com/flopwire/flopwire/blob/208f48b33f22d73f5d971e8b6859b4379158ada7/internal/digest/digest.go) and [server stale state](https://github.com/flopwire/flopwire/blob/208f48b33f22d73f5d971e8b6859b4379158ada7/internal/retrieval/retrieval.go). Report index coverage explicitly. |
| Entire preserves local squash links | [Tagged tests][entire-squash]. Hooks were source-reviewed, not executed here. |
| A checkpoint link can lack readable final context | [Push queue][entire-push] and [refs backfill][entire-refs]. Separate the link from source availability. |
| Plain Git can discard trailers during squash | The POC below. Keep the caller-side PR fallback. |

**Still unverified:** lookup performance, pagination correctness, and the new
index's behavior under source or access changes. These require implementation
and local/server tests before release.

The local POC used Git 2.55.0 in a synthetic repository with hooks and signing
disabled and no remotes. All five assertions passed:

- Squashing two topic commits produced a new commit ID.
- The original commits were not ancestors of the squash commit.
- An explicit squash message omitted their checkpoint trailers.
- Cherry-picking retained a trailer despite a changed committer and commit ID.
- An ordinary rebase retained a trailer despite a changed commit ID.

This tests plain Git. Entire's local hooks add squash preservation. No Entire
runtime, hosted-merge, credential, or production-performance POC was run.
Scratch artifacts were removed.

To reproduce, create two trailer-bearing commits on `topic`, then run
`git merge --squash topic` and `git commit -m 'feature'`. Inspect the message and
ancestry. On separate branches from the original base, cherry-pick one commit
and rebase the topic onto a new base; inspect their messages and IDs.

</details>

[entire-persistent]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/checkpoint/persistent.go
[entire-squash]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/manual_commit_squash_test.go
[entire-hooks]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/manual_commit_hooks.go
[entire-install]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/hooks.go
[entire-push]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/manual_commit_push.go
[entire-refs]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/checkpoint/refs_store.go
[github-squash]: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/configuring-commit-squashing-for-pull-requests
[agentboard-extractor]: https://github.com/gbasin/agentboard/blob/ab34776d7d1a23fdf55bf094f41af4f8300f21db/src/server/prExtractor.ts
[agentboard-info]: https://github.com/gbasin/agentboard/blob/ab34776d7d1a23fdf55bf094f41af4f8300f21db/src/server/prInfo.ts
