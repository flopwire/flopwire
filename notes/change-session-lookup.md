# Find the sessions behind a change

Proposal, 2026-10-13. Source baseline: `208f48b`. Production lookup is not
implemented. The [earlier experiment](https://github.com/flopwire/flopwire/pull/252)
found useful candidates by following original PR commits after squash merges.

## Recommendation

Add a read-only lookup from commit IDs to captured session evidence. Return
each matching session with the command/result address that explains the match.
Keep GitHub authentication with the calling agent. Keep search lexical and
coordination unchanged.

Entire's durable commit-to-context link is worth borrowing. Its Git hooks,
checkpoint refs, and publication queue solve a larger capture and distribution
problem than this lookup needs. Flopwire already captures and stores sessions;
it can derive searchable commit events from those records.

| Design choice | Benefit | Cost or limit |
|---|---|---|
| Internal commit-event index | Direct evidence addresses; no commit-message changes | A rebuildable projection needs local/server migrations, backfill, and deletion handling |
| Several sessions per change | Includes separate contributors and backports | Caller still has to inspect which session answers its question |
| Agent supplies original PR commits | Uses the agent's existing GitHub access | Squash lookup can require a GitHub call and a second Flopwire call |
| Evidence labels instead of an owner score | States what the captured history supports | Some results remain ambiguous |

## Agent interface

Proposed tool: `flopwire_change(repo, commits, limit?, cursor?)`. Require a
resolved repository and one or more full commit IDs. Bound the list to 100 IDs
per request; the caller batches larger PRs. Start with a default page of ten
sessions. These limits are provisional until the implementation is measured.

A dedicated tool keeps lineage, evidence, and coverage out of the already broad
`sessions` filter interface. It adds one tool to discovery, but gives this task
one predictable result shape. Include the caller's own session and descendants:
historical change lookup should not silently exclude them.

Typical flow:

1. Agent supplies the commit it is investigating.
2. Flopwire returns matching sessions, compact evidence, and addresses for `read`.
3. If the agent knows this was a squash merge, it uses `gh` to obtain the PR's
   original commits and submits those IDs together.
4. Agent reads the relevant evidence. Contact remains a separate decision.

The daemon and server receive no GitHub token and do not invoke `gh`. A supplied
commit list is a search input. Flopwire does not assert that the commits belong
to a PR or authenticate the caller's claimed lineage. The calling agent keeps
the GitHub result as its source for that relationship.

Each result includes the session, matched input commit, observed hash, operation
(`commit`, `cherry_pick`, or `revert`), and command/result read addresses. A quiet
commit resolved by a later HEAD command links both events. Report a short hash
as a prefix match even if it matches only one submitted ID; uniqueness within
the submitted list does not establish uniqueness in the repository.

Separate missing evidence from missing results: show an unknown command
repository, unavailable source, unfinished backfill, or partial page explicitly.
PR mentions may remain available through ordinary grep; omit them from this
tool's commit results. Do not mix weaker mentions into an apparently successful
commit lookup.

## Implementation shape

Extract events using the existing commit parser, preserving command/result
provenance before the digest reduces it to a capped SHA list. This requires
extending the shared parser to emit events; do not build a second shell parser.
Preserve operation type and distinguish a direct commit result from a later
HEAD reveal. Existing parser gaps remain gaps: wrapper scripts, absent output,
and unsupported shell shapes must not produce invented commit evidence.

Store events as a derived projection of retained transcript rows. Key events by
source identity/generation and source message identity, with separate addresses
for the original call, result, and any later reveal. Index observed hashes and
visible conversation identity. Short prefixes need bounded prefix matching;
query-plan and scale checks are required before finalizing that index.

Keep the observed command directory separately from the session's repository.
Only label a command repository known when captured placement evidence supports
the mapping. A current filesystem path or present-day Git remote cannot prove
what repository occupied that path when an old command ran. Unknown mappings
remain explicit. Repository filtering initially scopes sessions using existing
placement rules; it does not promise to discover every cross-repository command.

Local and server ingestion use the same event extractor. Reparse replaces a
generation's events idempotently. Redaction, superseding, deletion, and access
filters remove or hide associated evidence under the same rules as its source.
The projection is rebuildable from retained source records; it is not a new
backup contract. Do not cache raw evidence in pagination tokens.

Publish the event and its source generation atomically within each index. Page
in stable event order with an ingestion cutoff: later appends belong to a new
lookup and do not restart existing pages. Replacing or retracting evidence
invalidates affected pagination; return a restart condition rather than silently
skipping results. Bind cursors to the query and visibility scope, and recheck
access on every page. Finalizing this cursor contract requires an implementation
spike. Report visible-index coverage, capture/backfill lag, and truncation
separately. Completing a scan never establishes complete history.

## Why not require Entire-style checkpoints?

The v0.11.4 source supports multiple sessions per checkpoint and multiple
checkpoint links per commit. Local squash hooks inherit existing links, and
rewrite hooks handle amend/rebase mappings. That is stronger than inferring a
session from a branch name. See [persistent sessions][entire-persistent],
[squash tests][entire-squash], and [rewrite hooks][entire-hooks].

Those links still have independent failure states. Commit hooks can fail without
failing the commit. Checkpoint transport can fail while a code push succeeds.
Checkpoint refs can later receive transcript backfills. A link therefore does
not imply complete or available context. See [hook generation][entire-install],
[push handling][entire-push], and [refs storage][entire-refs].

GitHub permits squash messages containing only the PR title or its description.
Inference: a developer-machine squash hook cannot guarantee trailer retention
in a hosted merge. Follow the PR's original commits when needed, retaining that
provider evidence outside Flopwire. [GitHub squash settings][github-squash]

## Delivery and remaining proof

1. Extend the parser to emit source-linked commit events. Test quiet commits,
   failed compound commands, directory changes, cherry-picks, and reverts.
2. Add local/server projections with lifecycle parity. Validate reparse,
   redaction, deletion, access changes, backfill, and interrupted ingestion.
3. Expose the bounded lookup and document the agent-owned GitHub fallback.
   Measure query plans, output size, and latency before fixing limits.

The first implementation should preserve existing digest behavior. Fixing its
summary wording or changing which outside-directory commits it includes is a
separate decision. No Git hooks, new credentials, owner selection, or automatic
messages are part of this proposal.

<details>
<summary>Hand-computed state transitions</summary>

Shape 3: walk the requested lookup against current state, then identify the
state it lacks. All names and commit symbols below are synthetic.

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
<summary>Stress-test evidence and POC</summary>

| Claim | Impact if false | Strongest evidence | Missing evidence | Status | Next action |
|---|---|---|---|---|---|
| A digest match identifies the producing repository | Wrong attribution | [Parser tests](../internal/digest/commits_test.go) intentionally count outside-directory SHAs | Per-event placement is discarded | contradicted | Preserve location and uncertainty |
| Digests are exhaustive and current | False negative presented as certainty | [Digest caps](../internal/digest/digest.go); [server stale state](../internal/retrieval/retrieval.go) | Public result lacks digest freshness | contradicted | Derive event projection; report its coverage |
| Entire local squash drops all original links | Needlessly reject a useful approach | [Tagged squash tests][entire-squash] preserve links | Hooks not executed here | contradicted | Borrow relation semantics |
| A checkpoint pointer guarantees readable final context | Dangling or partial evidence | [Push queue][entire-push], [refs backfill][entire-refs] | Runtime Entire failure injection not run | contradicted | Separate link from source availability |
| Plain Git always retains checkpoint trailers | Lost lookup after squash | Synthetic Git POC below | Hosted merges and Entire hooks not executed | contradicted | Keep caller-side PR expansion |
| Event lookup can be bounded without missed pages | Incomplete results or costly scans | Proposed generation and cursor contract | Index/query implementation and measurements | unresolved | Query-plan, pagination and scale tests before release |
| Projection obeys source lifecycle/access rules | Stale or inaccessible evidence exposed | Existing source lifecycle provides integration points | New projection has not been implemented | unresolved | Required local/server lifecycle tests |

Local POC, 2026-10-13, Git 2.55.0: an isolated synthetic repository, hooks and
signing disabled, no remotes. It created two topic commits containing checkpoint
trailers, squashed them with an explicit new message, cherry-picked one with a
different committer, and rebased the topic onto an unrelated new base.

All five assertions passed: the squash had a new SHA; its original commits were
not its ancestors; its explicit message omitted their trailers; the cherry-pick
retained a trailer despite a changed committer/SHA; the ordinary rebase retained
a trailer despite a changed SHA. This tests plain Git only. Entire's local hooks
add squash preservation beyond that baseline. Scratch artifacts were removed.

Reproduce the behavior with two trailer-bearing commits on `topic`:
`git merge --squash topic`, then `git commit -m 'feature'`. Inspect the resulting
message and ancestry. On separate branches from the original base, cherry-pick
one commit and rebase the topic onto a new base; inspect messages and SHAs.

No production-performance, credential, hosted-merge, or Entire runtime POC was
run. The unresolved implementation claims above remain release requirements.

</details>

[entire-persistent]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/checkpoint/persistent.go
[entire-squash]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/manual_commit_squash_test.go
[entire-hooks]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/manual_commit_hooks.go
[entire-install]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/hooks.go
[entire-push]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/strategy/manual_commit_push.go
[entire-refs]: https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/checkpoint/refs_store.go
[github-squash]: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/configuring-commit-squashing-for-pull-requests
