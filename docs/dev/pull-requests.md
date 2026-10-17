# Pull request policy

The short form is in [CONTRIBUTING.md](../../CONTRIBUTING.md#pull-requests).
This page is the full policy.

## Branches and scope

Create each bounded change in a fresh worktree on its own branch. Start
independent changes from current `origin/main`. Preserve unrelated work.
Keep one reviewable PR per worktree. Do not combine unrelated refactors with
protocol changes.

Open a draft PR while the change is in progress. Check that the GitHub diff
contains only the intended change. Mark it ready when the implementation and
required evidence are complete.

For a dependent change, base the child PR on its parent branch. Record the parent
PR and its relevant commit in the child description. After the parent is
squash-merged, transplant only the child commits onto current main:

```sh
git fetch origin
git rebase --onto origin/main <last-parent-commit> <child-branch>
git diff --stat origin/main...HEAD
git diff origin/main...HEAD
```

Use the last parent commit included in the child branch as the rebase boundary.
Resolve conflicts and inspect the full diff. Push the rewritten child branch
with `git push --force-with-lease`. Retarget its PR to `main`. Refresh its
description and rerun affected checks. Merging main into an old child branch
does not remove inherited parent commits after a squash merge.

## Titles and descriptions

Use a conventional PR title with a concrete description, such as
`fix(search): report incomplete scans` or `feat(web): show message threads`.
The title becomes the squash commit subject and drives release-please.

Every authored PR has two sections:

- **What changes and why:** explain the concrete problem, resulting behavior,
  and why the approach fits. Describe the final diff for a reviewer who has
  not seen the conversation.
- **Validation:** identify the tested commit, relevant commands and results,
  and material unverified behavior. Include environment details when they
  affect the result. Distinguish local checks from GitHub CI.

Add compatibility, security, migration, rollout, dependency, or visual sections
when the change needs them. Omit empty sections and boilerplate assurances.
Generated release PRs retain release-please's format and the existing release
qualification process.

## Evidence and review

Keep concise validation evidence in the PR body. Put detailed runs, later
results, and review history in dated PR comments or durable attachments.
Preserve earlier failures and material limitations when updating the summary.
Commit documentation for reusable guidance or qualification records that the
repository intentionally retains. Routine PR evidence does not need a separate
checked-in log.

Independent review is optional. When citing a review, record its method,
reviewed commit, findings, and resolution on the PR. Link the record from the
description. Refresh the record after material changes. A review does not
replace runtime verification.

## Visual changes

For GUI or visual changes, include a **Before and after** section with actual
running-app captures. Attach images to GitHub so they survive branch deletion.
Keep capture files local. Use synthetic data and exclude private traces and
credentials.

Show comparable scenarios, state, viewport, theme, and text size on both sides.
Identify the baseline and head commits, platform, and capture method. For a
stacked PR, use its parent revision as the baseline. Describe what changed
and why it matters. Use meaningful image descriptions.

For a new screen, show the previous route or empty state. For navigation or
temporal changes, show the relevant states. Add a recording when still images
cannot demonstrate the behavior. Refresh captures when later changes affect
the pictured behavior.

Keep the PR draft if capture is blocked. State the blocker. Mark it ready after
attaching the captures, or after the repository owner explicitly grants an
exception recorded on the PR. Generated or reconstructed screens do not count
as evidence.

## Merge and cleanup

Before merging, check the final GitHub diff, title, description, dependencies,
and evidence. Replace stale pending status with the actual result. Explain
which earlier checks remain applicable if the tested revision differs from
the final head. Rerun checks affected by later edits or branch updates.

Use squash merges. GitHub uses the PR title as the commit subject and the PR
description as its body. Main requires an up-to-date branch and passing `test`,
`e2e-sync`, and `perf-gate` checks, including for administrators. Required
approvals remain zero. Publishing or deploying remains a separate action.

After the PR merges, verify that its worktree is clean and that no session,
server, or validation process still uses it. Then remove it with
`git worktree remove <path>`. Retain worktrees with uncommitted or unmerged work
and report why. Remove abandoned worktrees only when the owner explicitly
abandons the work. Never force removal without explicit authorization.
