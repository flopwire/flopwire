# Tool-call trigram index upgrade

Migration `017_tool_call_trigrams.sql` adds `messages_tool_call_trgm_idx`.
It indexes only `kind='tool_call'`. The query adds the same constant only
when the requested kind list is exactly `['tool_call']`. Other kinds and
mixed-kind requests retain their existing query path. The index does not
set a startup-time bound or change regex verification, ordering, visibility,
or continuation snapshots.

## Existing database

Schedule and qualify the index build before the binary cutover. The migration
runner applies pending SQL in one transaction. Its normal `CREATE INDEX`
blocks writes during a build. Use the online prebuild below for an existing
shared database. Fresh databases can use the normal migration.

1. Verify the service database name, host, and port. Connect explicitly to
   that database. Record `current_database()` and the index baseline.
2. Check free space, write latency, WAL growth, replica lag if applicable,
   and query latency. Set stop limits for the build. A second GIN index
   adds storage and write work; the synthetic benchmark is not a capacity estimate.
3. Run this SQL in `psql` with autocommit enabled. Do not wrap it in a
   transaction. Set the target schema in `search_path` first.

```sql
SELECT format(
  'CREATE INDEX CONCURRENTLY messages_tool_call_trgm_idx ON %I.messages USING gin (text %I.gin_trgm_ops) WHERE kind = ''tool_call''',
  current_schema(), n.nspname)
FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace
WHERE e.extname='pg_trgm'
\gexec
```

4. Inspect the completed index. Require `indisvalid`, `indisready`, and
   `indislive`. Compare its definition with migration 017. The migration
   checks the target table, column, access method, operator class,
   collation, predicate, and index options. It refuses a wrong or invalid
   index with the same name. It does not replace that index.
   Do not run concurrent DROP, REINDEX, or other index DDL during adoption.
5. If the concurrent build fails, inspect the remaining index before retrying.
   Remove an invalid build only through the approved operational procedure.
   Do not use `IF NOT EXISTS` to conceal an incomplete build.
6. Cut over to the qualified binary. Migration 017 adopts the matching
   prebuilt index and records its ledger entry. Verify the actual prepared
   query uses the partial index. Replay ordinary and continuation requests.
   Measure read blocks, read time, startup latency, and write overhead.

## Rollback

Prepare and qualify a rollback binary before cutover. It must embed the same
migration set, including 017, and retain the previous retrieval behavior.
An older binary that does not know ledger entry 017 refuses startup. A
binary-only rollback to that older version is not compatible.

The same-schema rollback leaves the additive index in place. It preserves
writes made after cutover. Do not delete ledger entries or restore an old
database as a routine binary rollback. Removing the extra index, if needed,
is a separate approved operation after the query rollback.
