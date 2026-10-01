# Retrieval example capture

`pagination.jsonl` is a synthetic Claude Code transcript. `grep.txt` and `read.txt` are unedited output from the retrieval CLI built at `abda135`. These preserve the original positional headers for comparison with the intended format.

To reproduce from the repository root:

```sh
go build -o /tmp/flopwire-example-cli ./cmd/flopwire
task_capture_dir=$(mktemp -d)
mkdir -p "$task_capture_dir/projects/acme-app" "$task_capture_dir/empty"
cp landing/coordination-preview/fixtures/pagination.jsonl \
  "$task_capture_dir/projects/acme-app/0b7e2c1a-0000-4000-8000-000000000001.jsonl"
export FLOPWIRE_CONFIG="$task_capture_dir/config.json"
export FLOPWIRE_INDEX="$task_capture_dir/index.db"
/tmp/flopwire-example-cli agent run --once --no-sync \
  --claude-projects "$task_capture_dir/projects" \
  --codex-home "$task_capture_dir/empty" --devin-db -
/tmp/flopwire-example-cli grep -F next_cursor --include-self
/tmp/flopwire-example-cli read 0b7e2c1a/3026944:1 -B 1
```

Run in a separate shell to keep the fixture configuration isolated. No server is used. The messaging example is documented separately in `landing/agent-tools.md`.

## Grep comparison

`src/pagination.ts` is the synthetic source file for the comparison. Captures: `grep -n -F next_cursor src/pagination.ts` → `file-grep.txt`; `flopwire grep -n -F next_cursor --include-self` → `transcript-grep.txt`. The latter uses the index built above. The shared flags are `-n -F`; `--include-self` is specific to Flopwire.

## Branch-history capture

`api-change.jsonl` is a synthetic Claude transcript with a successful git commit tool result. Index it separately, using the same isolated setup as above. The earlier preview used this projection:

```sh
flopwire sessions --repo app --branch api-users --json |
  jq '.sessions[] | {session: .session_id, title, commits: .digest.commits}'
```

Output is saved in `branch-session.json`. The underlying session has full ID `79b2d8ef-0000-4000-8000-000000000001`; retrieval also returns an `address` with a unique short form. The example uses the full native ID for the presence match. The presence check demonstrates the JSON CLI contract and uses the full ID returned by presence. It is not a captured messaging CLI run.

## JSON retrieval captures

The earlier preview projected these working CLI responses with `jq`:

```sh
flopwire grep -n -F next_cursor --include-self --json |
  jq '.hits[] | {session_id, address, lines}'
flopwire read 0b7e2c1a/3026944:1 -B 1 --json |
  jq '.messages[] | {address, role, text}'
```

The captures are `transcript-grep.json` and `read.json`. They use the pagination index above. Messaging examples project the existing Peer and SendResponse schemas; their CLI capture is tracked in #55.

## Intended interface fixtures

The earlier captures above document the CLI before the output-format decision. They remain source evidence; their `jq` projections are no longer shown on the homepage.

The homepage now displays these direct responses:

- `intended-sessions.json`: lean `sessions` envelope with existing `session_id`, `agent`, `title`, `branches`, and `digest.commits` fields.
- `intended-peers.json`: presence contract using named fields and the same full session ID.
- `intended-send.json`: queued receipt contract. It confirms queue acceptance, not delivery.
- `intended-grep.txt` and `intended-read.txt`: readable transcript content with the intended labeled headers.

These are intended contracts, not captured default CLI output. The `sessions` JSON default and labeled headers need a follow-up PR. Presence and send need captures from the messaging CLI. The recipient hook must land before capturing the reply exchange. Re-capture after those changes and compare against these fixtures. See [the output-format decision](https://github.com/flopwire/flopwire/issues/55#issuecomment-5940281818).
