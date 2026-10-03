# Recover CASS-only history

Flopwire can upload selected CASS conversations to a shared server. It preserves
CASS's normalized text, roles, timestamps, metadata, and source attribution.
It does not reconstruct missing native transcripts. Search and read label the
evidence as `cass_recovery`. Text output says `recovered from CASS`.

The importer supports Claude Code, Codex, and Devin CLI. CASS's `tool` role does
not distinguish tool calls from tool results. The importer keeps that role and
uses kind `unknown`. It does not infer tool names or parent relationships.
Original CASS metadata and external IDs remain in the recovery export. External
IDs that form valid addresses remain the session IDs. Other IDs receive a stable
CASS namespace. The manifest records the address mapping. Multiple CASS records
can name the same native filename; the importer keeps those records distinct.

## Prepare the selection

1. Keep the original CASS database and raw mirror.
2. Create a consistent SQLite backup of the CASS database.
3. Ingest current native history from every participating machine.
4. Include archived Codex sessions and configured harness directories.
5. Compare CASS conversations with the native session inventory.
6. Write the CASS-only conversation IDs to a JSON array, such as `[42, 77]`.
7. Check unmatched current files before adding their IDs to the selection.

CASS external IDs can contain paths or provider prefixes. Compare native IDs
from source filenames when needed. Do not treat a parent session UUID embedded
in a subagent's directory as that subagent's identity. Preserve an audit of
selected IDs and reasons. Do not select current native conversations: importing
older normalized rows would add duplicate evidence.

## Export and upload

Create a new private export directory:

```sh
flopwire import cass --db /private/cass-snapshot.db \
  --origin mac-original --selection /private/cass-only-ids.json \
  --out /private/cass-recovery
```

The command reads the snapshot in a read-only transaction. It streams one
message at a time. It writes a versioned JSONL file for each conversation and a
SHA-256 manifest. It publishes the manifest after every export closes. It
refuses an existing output directory. An interrupted export has no completed
manifest. Keep the snapshot and repeat into a new directory.

Enroll a device for the recovery origin. Use a separate device from live native
collection. Set `FLOPWIRE_CONFIG` to its configuration file. Then upload:

```sh
flopwire import upload --dir /private/cass-recovery
```

The command verifies every checksum before uploading. Upload uses the normal
authenticated, chunked sync protocol and server collection policy. It keeps
acknowledgment state per server and device in the export directory. Repeat the
command after an interruption to resume. Keep the directory until verification
and backup complete. Server indexing is asynchronous. Upload success alone does
not establish import completeness.

## Verify before retiring CASS

1. Compare indexed conversation and message counts with the export manifest.
2. Check that no selected source is pending, failed, quarantined, or refused.
3. Read representative sessions from each recovered harness and origin.
4. Check text, timestamps, source paths, and the recovery label.
5. Read raw records and confirm they return normalized CASS evidence.
6. Repeat the upload and confirm that indexed counts do not increase.
7. Back up PostgreSQL and object storage with `flopwire backup`.
8. Verify and restore the backup into empty test targets.
9. Check search and read from both Macs and every agent integration.
10. Stop CASS's watcher and replace its agent instructions and integrations.

Keep the original CASS snapshot and raw mirror as cold recovery archives.
Keep unsupported provider histories in that archive. Deleting the CASS binary
or its only database copy is a separate action from retiring its watcher.

Recovery exports have no local-index integration. Use the shared server to
search imported history. Collection rules and credential authorization still
apply. The server redacts recovery bytes and indexed text like native evidence.
Binary CASS metadata remains opaque base64 evidence; it is not decoded into
searchable text or interpreted as native tool structure.
