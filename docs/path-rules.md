# Path rules

Path rules keep sessions out of the local index or off the server. A
path rule matches where a session ran. It has one of three modes:

| Mode | Local index | Upload |
|---|---|---|
| `allow` (default) | yes | yes |
| `local` | yes | no |
| `deny` | no | no |

## Write your own rules

To add your own rules:

1. Open `path-rules` in the config directory (beside `config.json`).
2. Write one rule per line, as `MODE PATTERN`. A line with only a pattern
   is a `deny` rule. Lines that start with `#` are comments.
3. Save the file. The agent reads it again at the next sweep.

```
# never index or upload
deny ~/personal
# index here, never upload
local ~/clients/acme/**
# any directory named secret-client, at any depth
deny secret-client
# every repository of the acme organization on GitHub
deny repo:github.com/acme/*
```

## How rules match

- A path pattern covers the directory it names and everything below it.
- `*` and `?` match within one path segment. `**` matches across
  segments. `~` is your home directory.
- A `repo:` pattern matches the repository's `origin` remote, written as
  host/owner/name. `https://github.com/acme/web.git` and
  `git@github.com:acme/web.git` are both `github.com/acme/web`.
- Matching ignores case. A path is also matched with its symlinks
  resolved. For example, on macOS `deny /tmp/x` also covers a session that
  recorded `/private/tmp/x`.
- When rules disagree, the most restrictive rule wins (`deny`, then
  `local`, then `allow`).
- `config.json` can also hold rules, as a `"denylist"` array of strings
  in the same syntax. The agent adds them to the `path-rules` file.
- An admin sets rules for everyone on the server (`path_rules` in
  `PUT /v1/admin/policy` or the console's Collection policy page, written
  `local:PATTERN`, `local:repo:PATTERN` or a bare deny pattern). The
  agent fetches them at start and every 10 minutes, and keeps the last
  copy in `admin-path-rules.json.<authority hash>`. The hash identifies
  the normalized server and enrolled device. A credential without a device
  ID uses a token fingerprint. An offline start uses valid saved rules for
  that same authority, including an empty rule set. Saved rules do not expire.
  Your rules can make an admin rule stricter but not looser.
- If connected rules are missing, unreadable, or belong to another authority,
  collection and uploads pause until the server supplies valid rules. Existing
  indexed data stays in place. Queued uploads retry after rules become known.
  Cache write errors appear in the log; fetched rules remain active for the
  current process, but an offline restart may pause if those rules were not
  saved. Each authority's saved rules remain separate. Unowned cache files
  are left untouched and require a fresh fetch. Standalone and `--no-sync`
  collection use local rules without a cached administrator floor.
- The server also applies the admin rules when it parses an upload. It
  does not store a session that an admin `deny` or `local` rule covers.
  The server sees only the directory and remote that the transcript
  recorded, so a rule on a main checkout does not cover a worktree there.
  The agent is still the primary check. `flopwire agent status` lists the
  sources that the server refused.
- The agent reports your home directory to the server with each upload.
  The server uses it for `~` in admin rules.
- When an admin changes the rules, the server hides the stored sessions
  that the new rules cover. Search and read no longer show them. If the
  rules change again within seven days and no longer cover a hidden
  session, the server restores it. After seven days, or when an admin
  confirms the purge (`flopwire admin policy purge --yes`), the server
  deletes them permanently.
- A new `deny` rule removes the matching sessions from the local index.
  Your own rules never reach the server, so copies already uploaded stay
  there unless an admin rule covers them. `flopwire agent status` and
  the agent log name the sessions; ask the owner or an admin to delete
  them there.
- A session can move into another directory after it starts. The agent
  applies the most restrictive rule across every directory the session
  names. When a later directory moves an uploaded session to `local` or
  `deny`, the agent stops the upload and asks the server to delete the
  session and its subagents. The server keeps a tombstone, so the session
  does not come back, and audits the request as `conversation.withheld`
  with the rule. The server records it even before an upload commits. A
  member's enrolled credential with read and upload scopes withholds all
  of that member's device copies. Minted tokens, service credentials, and
  upload-only device credentials withhold only their bound device's copy
  and its subagents. A failed request, including an older server's `404`,
  stays owed and retries at each policy refresh and at start.
- If you remove a `deny` rule, the agent indexes those sessions again.

## Where a session ran

The agent finds these facts when it first sees a session:

- the working directory the transcript names;
- the git worktree root that holds it;
- the main checkout root. For a linked worktree (`git worktree add`), this
  is the checkout the worktree belongs to. For a worktree of a bare
  repository, it is the bare repository's directory;
- the `origin` remote, normalized.

A path pattern is checked against the working directory, the worktree root
and the main checkout root. A `repo:` pattern is checked against the
remote. A rule on `~/Code/app` therefore also covers the worktree
`~/Code/app-fix-login`.

The agent reads the git files (`.git`, `commondir`, `config`) directly and
does not run git. It stores the result in the local index, and keeps it
when the index is rebuilt. Rules still match a session after its worktree
is deleted, and a rule change is checked against the stored facts. When
the rules change, the agent resolves the facts again for each directory
that still exists, so it sees a remote that was added. It only fills in a
main checkout or remote that was unknown. It never replaces a stored one,
because the directory can be deleted and made again as another
repository.

A relative working directory (for example `.`) does not count as a
directory.

The same facts decide which repo a session is on for `--repo` in
`sessions`, `grep`, `search` and `peers`. A session's repository is its
main checkout (for a worktree of a bare repository, the bare repository)
and its remote. All worktrees of one repository are then one repo, with
or without a remote. Two repositories with the same name stay two repos.
A session in a deleted worktree stays on its repo through the stored main
checkout, or through the one that the recovery pass below records. A
session with only candidates is on no repo by its candidates. It still
matches by its directory. See [search.md](search.md#which-repo).

## Sessions in deleted worktrees

The agent can first see a session after its worktree was deleted. Then git
cannot give its main checkout. The agent does not use a repository above
the deleted directory that still exists. For example, a home directory
kept in git (dotfiles) is not the repository of a deleted worktree under
it. A background pass looks for the checkout
among the repositories the agent already knows (the main checkouts of
other sessions). It records a main checkout only when exactly one
repository fits these signals:

- `remote`: the session recorded a git remote, and exactly one local
  checkout has that `origin`. A rule on `~/Code/app` then covers an old
  Codex session from a deleted `app` worktree.
- `branch`: a branch the session recorded (not `main`, `master` or
  `HEAD`) is a branch, a remote branch or a reflog checkout of the
  repository.
- `worktree-add`: a session in a live checkout ran `git worktree add` for
  the directory. With `git -C DIR`, DIR's repository counts only when the
  agent already knows it.
- `commit`: a commit the session printed, such as `[fix abc1234]`, is in
  the repository.

When more than one repository fits, the agent records no main checkout.
It keeps every repository that fits as a candidate and applies the rules
once with each candidate as the main checkout. The most restrictive result
wins. A transcript can claim anything, so a false claim can only make the
result stricter. Candidates stay across later checks.

The agent does not upload such a session until the pass has checked it.
If a `deny` rule could match, the agent does not index it until then
either. A session in a temporary directory with none of these signals
keeps its working directory only. The agent checks a session that it did
not place again after a day.

`flopwire agent status` shows how many sessions each method placed: `cwd`,
`worktree`, `folder`, `remote`, `branch`, `worktree-add`, `commit` or
`unplaceable`. `ambiguous` counts the sessions that have candidates. Each
of them is also counted under its method.

When the transcript does not name its directory:

- For Claude Code, the project folder name stands for it. For example,
  `~/.claude/projects/-Users-me-Code-app` is `/Users/me/Code/app`. The
  name is lossy, so the agent looks for the longest path that exists.
- For old Codex rollouts, the git remote they recorded places the session.
- The agent waits 2 minutes for a new transcript to name its directory
  before it uses these fallbacks. It does not upload the session while it
  waits. If a `deny` rule could match, it does not index the session
  either.

## Sessions with no place

A session is unplaceable when the agent finds no directory and no remote
for it. The `unplaceable` setting decides what happens to it:

| Value | Local index | Upload |
|---|---|---|
| `local` (default) | yes | no |
| `upload` | yes | yes |
| `exclude` | no | no |

To set it on a device, add `"unplaceable": "upload"` (or another value) to
`config.json`. Restart the agent.

An admin sets a floor in the server policy (`unplaceable` in
`PUT /v1/admin/policy`, or the admin console). The more restrictive of
the two settings applies.
