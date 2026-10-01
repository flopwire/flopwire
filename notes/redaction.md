# Redaction design (spec §6.6, decision 4)

Status: built on `feat/redaction`. This note amends spec §6.6. The owner's local index stays unredacted (decision 4). Redaction applies to every byte that leaves the device and to everything the server stores or serves.

## What is redacted

Secrets only in phase 1. Rules live in `internal/redact`, versioned as `RulesVersion`:

- Vendor tokens with fixed shapes: AWS access key ids, GitHub (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), GitLab, Anthropic, OpenAI, Slack tokens and webhooks, Stripe, Google API keys and OAuth secrets, npm, PyPI, Hugging Face, SendGrid, DigitalOcean, Tailscale, Databricks, Linear, age keys, and Flopwire's own device and invite tokens.
- Private key blocks (PEM and OpenSSH), both with real newlines and JSON-escaped (`\n`).
- JWTs.
- Credentials in URLs (`scheme://user:pass@host`, where only `pass` is masked; stock dev passwords and loopback hosts are exempt), `Authorization`, `Cookie`, `X-Api-Key` headers, and Azure `AccountKey=`.
- Assignments in env dumps, `.env` files, JSON and YAML whose key names a secret (`*SECRET*`, `*TOKEN*`, `*PASSWORD*`, `*API_KEY*`, `*ACCESS_KEY*`, `*CREDENTIAL*`, and similar). The value must be at least 8 characters, not a placeholder (`changeme`, `<...>`, `${...}`, `xxxx`, repeated characters, a prior marker) and not a code reference (`process.env.X`, `os.getenv(...)`, `$VAR`). It must also pass an entropy floor.

Sources surveyed: gitleaks (MIT, 222 rules; the vendor shapes follow its config, and the ported rule file carries a header naming the commit), agentsview `internal/secrets` (MIT; 16 rules in "definite" and "candidate" classes, redaction on search snippets only), and entireio/cli `redact` (MIT; betterleaks, which is a gitleaks fork, plus an entropy sweep over every token of 10 or more characters, connection strings, and opt-in PII covering email, phone and address). Trufflehog is AGPL, so its code is not used. Entire's entropy sweep over every token would mask commit SHAs, UUIDs and base64 content across the corpus. Here, entropy only gates values that sit next to a secret keyword.

PII is not in phase 1 (question Q1).

## Where it runs

It runs in both places, and both passes use the same package.

1. **Device, before chunking.** `devicesync` reads every source through `redact.ReaderAt`, and that covers the capture scan, the tail, payload re-reads and salvage. Chunks, hashes, the spool and the S3 archive therefore hold redacted bytes. The raw original exists only in the harness files on the device, which the local index reads directly.
2. **Server, at parse and at read.** `ingest` parses through the same wrapper, and `retrieval.fetch` serves `read --raw` through it too. Bytes from a device that already redacted them pass through unchanged, because markers never match a rule. Bytes that were uploaded unredacted come out masked in rows and in raw reads: the two-laptop archive, an old agent, or a rule that shipped after the upload. The server counts what its own pass catches, so a nonzero count points at a device miss.

**Length-preserving markers.** A match of n bytes becomes exactly n bytes: `[REDACTED:<rule>:<h8>]` padded with `*`. When that marker does not fit, it drops to `[REDACTED:<rule>]`, then to `[REDACTED]`, and then to `*` repeated n times. `h8` is the first 8 hex digits of SHA-256 over a domain tag and the secret, so two hits of the same token correlate. Password-class rules (keyword assignment, URL password, auth headers) omit the hash, because a guessable password plus a 32-bit hash lets anyone confirm a guess (question Q2). Markers contain no `"`, `\` or control bytes. Match edges that fall inside a JSON escape widen to cover the whole escape, so every JSONL line still parses.

Keeping the length has these consequences:

- Every byte offset is the same in the raw file, the archive and the local index. A message address, `byte_offset`/`byte_len`, and `read --raw <offset,len>` point at the same range on the device and on the server. The server's copy shows markers where the device's copy shows the secret.
- Content addressing still holds, because the chunk hash is the hash of the redacted bytes. Two devices that hold the same secret produce the same redacted chunk.
- Redaction is a pure function of the line. In `.jsonl` sources and Devin exports, each line is redacted on its own. A line longer than 1MB is split at fixed offsets from the line start, with 64KB of overlap. Other text files (Claude `tool-results/*.txt`, `meta.json`) are redacted as a whole, so a multi-line PEM block is caught. Binary companions (images, PDF, Office files) are not redacted.
- The device withholds a partial last line of an append-only source until the line ends with a newline or the file goes idle (`SealAfter`). This stops a half-written secret from being uploaded before the rest of it arrives. Limit: suppose a file idles, the partial line is sealed, and then the same line continues. A secret that straddles the seal point is judged on each side separately.

## The two-laptop archive (decision 4)

Recommendation: wipe the server and re-sync from both laptops with redacting agents before the first team user joins. Flopwire is pre-release, so we build no re-chunk tool for our own old format. Until then, the server-side pass already masks rows and raw reads. The only exposure left is the bytes at rest in S3 and in backups. Cost: sessions whose local file has since been deleted (Claude's 30-day cleanup) exist only in the archive and would be lost. Alternative: scope the old sources to their owner, which needs a new visibility flag on sources (question Q3).

## Redaction record

The redacted bytes themselves carry the marker, so `REDACTED:github-token` and `REDACTED:github-token:1a2b3c4d` can be searched. Each flush sends `{rules_version, counts per rule}` for the source's current generation, and the server stores this on `generations` (`redaction_rules`, `redactions`). The server pass stores its own counts per rule on `source_parse_state.server_redactions`. `flopwire agent status` prints the device totals. `/v1/admin/status` returns device totals, server-pass totals, the number of sources with redactions, and the number of sources uploaded without redaction. No secret, reversible form, or match text is stored or logged anywhere.

## False positives and misses

- Built-in allowlist: documented example keys (`AKIAIOSFODNN7EXAMPLE`, values ending in `EXAMPLE`), placeholder values, and markers.
- A false positive costs readability, not data: the owner's local index and harness files keep the original.
- A miss found after the fact: ship a rule, which bumps `RulesVersion`. The server's read-time pass then masks the secret in every raw read immediately, and a reparse (question Q5) masks the rows. To remove the bytes at rest, the owner or an admin deletes the conversation (the existing deletion and tombstone path), and the next upload of a still-live file is redacted under the new rule. Rotate the secret regardless.
- Per-repo and admin rules (extra deny patterns, allowlisted values) follow the path-rule channel (D18) as a follow-up (question Q4).

## Redacting a message after the fact

An owner (for their own messages) or an admin (for any message) redacts one message or a range of its lines. The command is `flopwire redact [--all-copies] [--admin] [--local] ADDRESS[:L1-L2]`. It calls `POST /v1/redactions` with the device credential, or `POST /v1/admin/redactions` with an admin's login session. It then applies the same tombstone to the caller's local index, through the running agent or directly when no agent runs. `L1-L2` are text lines as `flopwire read` numbers them. Without a range, the whole message is redacted.

1. **Targets.** The first target is the addressed row. The redaction also covers other versions of the same record: superseded rows with the same `(conversation, native_id)`. With `--all-copies`, it also covers every row with the same `content_sha`, which is the `+N copies` group. That group includes subagent copies, forks, and the same session archived from the user's other devices. The owner route limits the scope to the caller's own rows. The admin route covers the whole organisation.
2. **Rows.** In each target row, the redacted text becomes `[REDACTED:message]` padded with `*` to the same length, and `content_sha` is recomputed. `tsv` and the trigram index follow, because both derive from `text`.
3. **Raw bytes.** The row's record is `[byte_offset, byte_len)` in its source generation. A JSON tokenizer decodes each string value and keeps a map from decoded positions back to raw positions. JSON nested inside a string is walked the same way, so a Devin `chat_message` or Codex call `arguments` stays valid JSON. A line range masks every occurrence of the target lines inside decoded strings. A whole message masks every string value except structural keys (`type`, `uuid`, `parentUuid`, `sessionId`, `timestamp`, `id`, `role`). The masks are length-preserving and JSON-safe, like rule markers. If the target text is not found in the raw bytes (for example text that the parser assembled from several blocks), the whole record is masked as a fallback, and the result counts it. A row without a byte range (a Devin row, parsed from a rebuilt store) is found among the generation's lines: by the hidden lines, or for a whole message by its native id. For a Claude persisted tool output, the companion `tool-results` file is also masked by exact-text match.
4. **Chunks.** Each chunk that overlaps a mask is fetched, masked, and stored as a new content-addressed chunk. Every manifest entry that references the old hash, in any source or generation, is repointed to the new hash. The chunk bytes are identical wherever they appear, so the same chunk-relative mask applies everywhere. A provisional tail is rewritten in place. If the next device delta hashes the original prefix, the server requests the full tail and keeps the same generation. The uploaded tail is then repaired by step 5. `chunk_redirects(old → new)` records the substitution, and ingest applies it to later flushes, so a device that still holds the old chunk cannot upload it again. Its verified old body proves possession of the new chunk, so another user who shares the bytes still syncs. The old chunks move to `deletion_pending` under a deletion job (`kind = 'redaction'`). The existing purge worker deletes them from S3, and backups skip them, as they already skip deletion-owned chunks.
   Flush checks redirects again while holding chunk-row locks, both when reserving a body and before comparing or committing manifest entries. If redaction changed a redirect during the request, flush returns a retryable 503 before taking the old chunk away from its purge job or referencing it again. The retry resolves the replacement hash and proves possession in the usual way. These database locks do not span request-body reads or object-storage writes.
5. **Re-uploads with different chunking.** A rewritten file can split the same record into chunks that the redirect never saw. For that case the redaction also stores `redacted_lines(sha256 of the original raw line → masks)`. The evidence contains hashes and byte spans, never transcript text. New requests also store the record length, a stable-prefix hash, and a canonical masked-record hash. These recognize records with a mix of raw and already masked bytes, including a secret split between a chunk and a tail. Records without this proof support exact hashes and successive mask chains.
   After known message redactions exist, a changed flush records durable `archive_redaction_work` for its source and generation in the flush transaction. The parse worker scans from the earliest changed record, verifies matches, and uses the chunk rewrite and deletion path from step 4 before parsing. Each batch commits its rewrite and work checkpoint together. A concurrent append changes the work revision, so a stale worker retries instead of clearing new work. Companion sources and superseded queued generations are included. Existing unqueued archives are not scanned or backfilled. Parse and read masking remain in place while cleanup is pending.
   Storage grows by one work row per pending generation and a small hash proof per redacted record. Replacement objects coexist with old objects until purge finishes. Scanning streams records through a bounded buffer; a rewrite reads the affected chunks and writes replacement chunks. Failed replacement writes remain in the existing orphan ledger. The worker emits `archive.redaction.rewritten` with IDs and counts only.
6. **Audit.** `message.redaction.requested` and `message.redaction.complete` record the actor, the targets, the chunks rewritten and the job. They never record text.
7. **Local index.** The same rows are masked in the caller's local index, FTS included. The tombstone `(content_sha of the original text, session, native_id, line range)` is appended to a sidecar file beside the database (`index.db.redactions.jsonl`). The sidecar holds no text. Every later write of a matching row is masked before it is stored, so the mask survives a re-parse, a reindex, and an index rebuild that keeps the sidecar. The harness's own transcript file on disk is out of scope. So is a local index on another device: that device masks the rows when its own owner runs the command there.

## Performance

The device first-sync bar is about 19GB, and each live line must be handled in about 300ms. One case-insensitive Aho-Corasick pass over each segment finds every rule keyword. A rule's regex then runs only on a window around a hit, and the keyword-assignment rule is a procedural check at each hit. Measured on Gary's corpus (19.09GB, 20,622 files), redaction runs at 126MB/s on one core, so the full corpus takes 2m32s. The device redacts at capture and again when upload re-reads a chunk from the file. A live line costs microseconds. Local indexing is untouched.

## Open questions (with recommendations)

- **Q1 PII:** no PII in phase 1. Emails appear in every commit and in `git config`, and the team already knows its own names. Add opt-in email masking later if a customer asks.
- **Q2 Hash on password-class matches:** omit the hash. Keep it for vendor tokens, which have high entropy by construction.
- **Q3 Two-laptop archive:** wipe and re-sync before the first team user. The server-side pass covers the interim.
- **Q4 Custom rules and allowlist:** built-in rules only for now. Admin rules later ride the policy channel as floors, the same way path rules do.
- **Q5 Reparse on a rules change:** implemented. Applied rule-version stamps drive a paced background refresh from archived bytes. Refresh also masks all stored message versions, enrichment, titles, and digests. Historical archive bytes and backups remain untouched. See [versioned reparse](reparse.md).
- **Q6 Generic keyword rule:** keep it on. Use the entropy floor and placeholder filters, and tune from the corpus false-positive review.

- **Q7 At-rest rewrite of a re-uploaded redacted line:** implemented for future changed uploads through durable parse-worker repair. No historical scan or backfill. Cleanup is asynchronous; old objects remain until the purge worker finishes.

None of these blocks the build.
