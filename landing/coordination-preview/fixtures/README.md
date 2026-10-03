# Homepage output captures

Captured with the CLI built from main `2d7b552` on 2026-10-02. Files named
`intended-*` now contain direct CLI responses. The filenames are retained
so existing references keep working. Homepage JSON is indented for display;
fields and values are unchanged.

These captures use synthetic transcripts in an isolated local index and
message bus. Both sessions belong to the same person (`sender="own"`). The
capture script supplies presence and calls the hooks. It does not run coding
agents or demonstrate cross-person messaging. No active user sessions receive
messages, and no server is contacted.

The separate [recorded agent exchange](../../../notes/message-bus/exchange-capture/README.md)
ran Claude Code and Codex for real. That exchange is also same-person. Its
API agent used `git commit -q`, so its recorded commit list is empty. Do not
add a commit to that historical capture or describe it as a cross-person exchange.

## Reproduce

From the repository root:

```sh
go build -o /tmp/flopwire-homepage-current ./cmd/flopwire
python3 landing/coordination-preview/fixtures/capture.py /tmp/flopwire-homepage-current
```

The script creates a scratch directory under `/tmp`, sets isolated
`FLOPWIRE_CONFIG` and `FLOPWIRE_INDEX` paths, and sets explicit fixture caller
IDs. It indexes only the supplied Claude transcripts and a generated Codex
transcript. It runs the daemon with `--no-sync`, then stops it.

For presence, it retimestamps a working copy of `api-change.jsonl` to the
capture time. A synthetic Claude registry entry marks that session busy.
The Codex fixture has a `task_started` event. The original input fixtures
remain unchanged. Message IDs, timestamps, local username, device name,
and scratch paths vary between runs. Re-embed the resulting output after
recapturing; never hand-edit those fields to match an older example.

## Retrieval

`pagination.jsonl` supplies the pagination discussion. Captured commands:

```sh
flopwire grep -F next_cursor
flopwire grep -n -F next_cursor
flopwire grep -l -F next_cursor
flopwire search next_cursor
flopwire read 0b7e2c1a-0000-4000-8000-000000000001/3026944:1 --messages-before 1
flopwire grep -n -F next_cursor --json
flopwire read 0b7e2c1a-0000-4000-8000-000000000001/3026944:1 --messages-before 1 --json
```

Text captures: `grep.txt`, `transcript-grep.txt`, `grep-files.txt`, `search.txt`,
`read.txt`, `intended-grep.txt`, and `intended-read.txt`. JSON captures:
`transcript-grep.json` and `read.json`. No `jq` projection is applied.

Headers lead with the full session ID and use `key=value` metadata. Values
containing spaces or quotes are JSON-quoted. Intent or title is last. Read
context uses `--messages-before` and `--messages-after`, not `-B` or `-A`.

`src/pagination.ts` is the synthetic source file in the grep comparison.
`file-grep.txt` contains `grep -n -F next_cursor src/pagination.ts` output.
The shared flags are `-n -F`. Flopwire excludes the caller’s own session by default; `--include-self` includes it.

## Session history

`api-change.jsonl` includes a plain `git commit` tool call and its successful
`[api-users a81f3c2]` result. Captured commands:

```sh
flopwire sessions --repo app --branch api-users
flopwire sessions --repo app --branch api-users --text
```

`branch-session.json` and `intended-sessions.json` contain the complete default
JSON response. `sessions-text.txt` contains the readable alternative.
Commit IDs are at `.sessions[].commits`. A projection, if needed for analysis,
is `jq '.sessions[] | {session_id, title, commits}'`. Deeper digest metadata
requires `--detail`; the homepage does not project or request it.

A quiet `git commit -q` can leave the commit list empty ([#80](https://github.com/flopwire/flopwire/issues/80)).
Read the session's outline and tool results when attribution is incomplete.
A branch or title alone does not prove ownership.

## Messaging

The API session has full ID `79b2d8ef-0000-4000-8000-000000000001`.
The client session has full ID `4c19e0d2-0000-4000-8000-000000000001`.
The script captures `peers --repo app`, a request, recipient hook output,
a reply, sender hook output, and the resulting inbox thread.

- `intended-peers.json`: complete presence response.
- `intended-send.json`: complete request receipt, including `next` guidance.
- `delivered-request.json`: recipient's `PostToolUse` hook output.
- `reply-receipt.json`: the reply receipt.
- `delivered-reply.json`: sender's `PostToolUse` hook output.
- `intended-inbox.json`: the complete thread, newest first.

The request receipt confirms queue acceptance. The answer is a received
message with matching `reply_to` and `thread_id`. Messages arrive at a tool
boundary or with the human's next prompt. They never wake an idle session.

Cross-person messages are held until the recipient accepts the sender on the
console Messaging page or with `flopwire accept USER` and their password.
This behavior is implemented, but these files do not capture it.

Decisions: [#55](https://github.com/flopwire/flopwire/issues/55).
Tracker: [#73](https://github.com/flopwire/flopwire/issues/73).
