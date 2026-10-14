# Embedded search adapter POC — 2026-10-14

Status: experiment in progress. This record does not qualify a production search engine.

This follows [the broad-regex experiment](broad-regex-poc-2026-10-14.md) and tests the integration cost that its isolated index benchmark omitted. The intended agent contract is a stable initial search snapshot, current authorization on each continuation, and a fresh search for newer messages.

The experiment uses main `883a467f427be88f05a58c2751a08f1cb30e6d86`, PostgreSQL 17.6, and embedded Zoekt `v0.0.0-20261014141728-4abc8196181a`. Production source and dependencies remain unchanged. All inputs are synthetic.

## Questions under test

- Can every snapshot message be covered by indexed candidates or an explicit fallback?
- Do the production Go matcher and authenticated read endpoint remain authoritative?
- Can bounded continuation handle retries, revocation, redaction, expiry, and concurrent requests without declaring a false negative?
- What do first-page and full traversal cost after authorization and original-body verification?

## Scope

The candidate accelerator handles a conservative finite ASCII literal language. Unsupported patterns scan the fixed snapshot. Short, NUL-containing, non-ASCII, oversized, and high-diversity bodies enter an explicit fallback path. Each generation owns fixed shard handles.

The continuation experiment supports a limited query/filter subset. It does not establish durable distributed cursors, crash recovery, cross-page deduplication, or capacity for a hosted team of 30 people. These remain production design gates.

## Evidence

Pending runtime results and independent review. Source and sanitized results will be archived alongside this record; temporary databases and indexes will be removed.
