# flopwire MVP scope

`flopwire` is not a CASS fork. It is an Apache-2.0 team companion around
[CASS](https://github.com/Dicklesworthstone/coding_agent_session_search), with
CASS treated as a pinned, replaceable search engine.

## Core architecture

```text
macOS/Linux collectors
  -> resumable raw segment uploads
  -> Go API
  -> Postgres metadata + S3/MinIO raw archive
  -> server-side CASS parsing/indexing
  -> central hybrid-search API
  -> CLI + MCP

TypeScript admin console
  -> users, devices, policies, audit, deletion, health
```

## Identity model

- One self-hosted deployment equals one organization.
- Users claim one-time invitations and create local password accounts.
- The server assigns the canonical user UUID.
- Each installation receives a revocable device credential.
- The authenticated uploader is the canonical author.
- Embedded names and emails remain untrusted metadata.
- Normalized Git remotes identify projects across machines.
- Service accounts represent CI and unattended agents.
- Service accounts are upload-only by default.
- Roles are `admin` and `member`.

## Corpus policy

- Automatically upload everything CASS discovers, minus denylist rules.
- Combine mandatory admin exclusions with private local exclusions.
- Upload raw traces without local secret redaction.
- Include trace-owned attachments, but never chase arbitrary workspace paths.
- Sync active sessions every 30-60 seconds as immutable hashed segments.
- Import all eligible historical traces on enrollment.
- Keep uploaded data indefinitely.
- Local deletion does not propagate.
- Only admins can permanently delete central traces.
- Every member can search the entire organization corpus.
- Audit all reads and writes.
- Server administrators are explicitly trusted with plaintext access.

Search returns trace chunks with exact provenance, not generated answers or
knowledge cards. Default retrieval is hybrid lexical and semantic search using
a local CPU embedding model. Filters include author, project, source, and time.

## V1 release bar

- Production-ready for up to 30 users.
- macOS and Linux collectors.
- Single-node Docker Compose.
- Postgres plus S3-compatible storage.
- Go services and CLI.
- CLI plus MCP retrieval.
- Admin-only TypeScript web console.
- Built-in backup and restore.
- Rebuildable CASS indexes.
- Structured logs and Prometheus metrics.
- Storage quotas that pause backfill before rejecting live uploads.

## Explicit risks

1. Raw, unredacted, organization-wide, indefinite storage makes this a
   sensitive company archive. The threat model must say this plainly.
2. A single read/write device token stored in a configuration file means theft
   of a developer home directory can expose the corpus. Restrictive file
   permissions, revocation, rotation, and redaction from diagnostics are V1
   requirements, but do not eliminate that risk.

