# Troubleshooting

This page tells you how to recover a device agent that stops with a
redaction error, and how to read extraction diagnostics. Stop the agent
before each recovery procedure. See [Stop the agent](agent.md#stop-the-agent).
For the files in the index directory, see
[Recover the local index](agent.md#recover-the-local-index). To measure
the agent's work, see [daemon diagnostics](agent-diagnostics.md).

## The agent stops: "index.db.redactions.jsonl is corrupt"

The agent does not open an index while a redaction is unreadable.

1. Stop the agent.
2. Copy the damaged file to a safe place:
   `cp index.db.redactions.jsonl ~/flopwire-redactions-damaged.jsonl`.
3. If `index.db.redactions.jsonl.prev` exists, copy it over the damaged
   file: `cp index.db.redactions.jsonl.prev index.db.redactions.jsonl`.
4. If no `.prev` file exists, open the file in a text editor. Remove only
   the line that the error names. Save the file.
5. Start the agent.
6. Run again each `flopwire redact` command that you ran after the copy
   that you restored. The file cannot show which message a removed line
   hid. If you are not sure, run all of them again. A repeated redaction
   of hidden text reports `nothing to redact`.

## The agent stops: "its key ... is missing", "the key does not match", or "redactions.key is corrupt"

The agent cannot read the redactions without the key.

1. Stop the agent.
2. Restore `index.db.redactions.key` from the backup that you made with
   this redaction file, if you have one. A key from another device or
   another backup does not match. Then start the agent. Stop here.
3. Without a backup, move both redaction files to a safe place:
   `mv index.db.redactions.jsonl index.db.redactions.jsonl.prev ~/`.
4. Run `flopwire agent run --rebuild-index --once`.
5. Start the agent.
6. Run again every `flopwire redact` command that you ran on this device.
   Until you do, search shows the text that you redacted.

## The agent stops: "apply redactions"

The agent could not apply a redaction to the stored rows.

1. Stop the agent.
2. Run `flopwire agent run --rebuild-index --once`. It indexes every
   transcript again, then exits.
3. Start the agent.

The rebuild removes the message rows, the conversations and the search
files. It keeps the sync state, the placements, the redaction files and
the key. Then it indexes every transcript again and masks each redacted
row as it writes it. Search is incomplete until the first pass ends. Use
the same procedure if the index is damaged in another way.

Do not add `--rebuild-index` to a login item or the launchd agent. The
agent would rebuild the index at each restart.

## Inspect extraction diagnostics

Run `flopwire diagnostics` to inspect the local index.
Run `flopwire diagnostics --server` to inspect the team server.
Add `--json` for machine-readable output.

The summary shows assessed and unassessed Claude and Codex JSONL sources.
It lists issue totals and up to 20 affected source IDs.
Use `flopwire diagnostics --source ID` to inspect one local source.
Add `--server` to inspect a server source ID.
Use `--index PATH` to select a local index.

`flopwire agent status` includes the local extraction summary.
`flopwire agent status --json` returns the control response as JSON.
Status limits diagnostic reads to two seconds. During capture or policy work,
Cowork status can be unavailable. Busy or failed diagnostics do not hide sync
and messaging health. JSON responses name unavailable sections in `unavailable`.
Treat counters in those sections as unknown. Run status again for a fresh report.
Run `flopwire diagnostics` for the complete local extraction report.
The server exposes the same reports at `GET /v1/diagnostics`.
Add `source_id=ID` to request one source.
The admin status response includes `diagnostics.extraction`.

### Read a report

A report records observations over consumed complete records. An incomplete
live tail waits for its next append. An assessed source with zero issues means
that extraction observed none. An unassessed source has no completed report.
Devin and opencode exports and companion files do not receive JSONL extraction reports.

Each report identifies its parser contract, raw generation, consumed byte
offset, and line number. It stores issue counts and at most eight line/byte
examples per issue code. It stores no transcript text or arbitrary error strings.

| Code | Meaning |
|---|---|
| `malformed_record` | A complete record could not be decoded as JSON. |
| `record_too_large` | A record exceeded the parser's materialization limit. |
| `field_type_mismatch` | A known field had an unexpected JSON type. |
| `unknown_record_type` | An unrecognized type was observed. This is informational. |
| `text_truncated` | Indexed text was capped by the effective extraction policy. |

A count describes affected physical records. It does not count lost messages.
One record can produce several messages or none. Intentional metadata and
recognized duplicate event mirrors do not produce issues.

### Understand report lifetime

Appends add observations to the current checkpoint. A rewrite or parser
contract change replaces the report with a full assessment. A retry does not
add the same observations twice. The latest report is stored beside the parse
cursor in SQLite locally and PostgreSQL on the server. Old report snapshots
are not retained. Raw transcript evidence follows the existing archive policy.

Source detail also counts indexed live messages with missing or truncated persisted
companion output. These counts come from current message metadata. They clear
when a reparse incorporates a repaired output. They are separate from the
cumulative JSONL observations. Companion files retain the existing written-once
assumption. Editing a previously readable companion alone does not guarantee
an immediate reparse.

Server diagnostics require a reader credential. Hidden or tombstoned evidence
is excluded. Each server read is audited. Local diagnostics use the index's
existing file permissions.
