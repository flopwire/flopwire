# Bus migration upgrade from 661a48b

This forward upgrade preserves durable data in a database whose ledger is
the contiguous prefix `001_schema.sql` through `010_cass_recovery.sql`.
It also accepts a database built from the current version of `009_bus.sql`.
Migrations 011 and 012 update presence and precede the bus upgrade in 013.
Migrations 011, 012, and 013 ship together in one release. The embedded
migration files must be numbered contiguously from 001 through 013.

## Recognition contract

The production 009 is the authentic file at
`git show 661a48b:migrations/009_bus.sql`. Its SHA-256 is
`fd7ce62752862b6b566472e248247a849f07a6a39ae7f892237aa4af8ae927d7`.
The regression fixture preserves those exact bytes at
`internal/store/testdata/009_bus_661a48b.sql`. Files 001–008 and 010 at
661a48b match their current bytes. Unit tests pin all ten current checksums.

The runner recognizes that historical 009 checksum only when the embedded
009 checksum is
`16150bac5be12b63ec029bcec5fc927956b7716c08a9fce292899b9c34db57f2`
and the embedded `013_bus_retention_upgrade.sql` has SHA-256
`bb979ebc3509eb893c50c02f97c01e6e6a4c0e4a0939b144b9bb4362cbc1668f`.
The additive migration contains the schema delta between the two known
009 versions. The runner keeps the historical ledger row, including its
checksum and application time. It records 013 with its own checksum.
Recognition remains necessary on later startups because the old checksum
stays in the ledger.

Every other checksum comparison stays exact. Unknown 009 checksums,
an edited embedded 009, and a missing or edited 013 are refused for the
historical database. The ledger must remain a contiguous prefix of the
sorted embedded files. A ledger with entries unknown to the binary is
refused. There is no skip-validation flag or ledger repair step.

## Schema delta and data contract

013 adds `attempts integer NOT NULL DEFAULT 1`, its positive-value check,
and nullable `last_at` when absent. Existing current-schema values remain
unchanged. A named check permits multiple attempts only for refused rows.
Current 009 already has that rule under a generated name; its redundant
check remains. A refusal can increment its counter and set `last_at`.
A queued message cannot acquire multiple attempts.

013 changes the reply foreign key to `ON DELETE SET NULL`. It preserves
existing reply links. Later retention can delete a parent while keeping
its reply. It adds the final-message retention index and the two bus audit
retention indexes when absent. It does not delete messages or audit rows.
Normal retention sweeps after startup follow the configured retention policy.

The existing migration transaction includes all pending SQL and ledger
inserts. Its advisory lock serializes callers. A failure rolls back the
entire pending set. A second startup applies nothing. The SQL in 013 can
also be replayed against either supported schema without changing data.

## Central PostgreSQL checks

Run these commands from the release checkout.
Set `FLOPWIRE_TEST_DATABASE_URL` to a disposable PostgreSQL test service
with permission to create databases. The tests create and remove their own
databases. Do not use a production connection. An unset variable causes
the PostgreSQL tests to skip; a skip does not validate the upgrade.

Run the lightweight checks without a database connection:

```sh
env -u FLOPWIRE_TEST_DATABASE_URL go test ./internal/store -run '^TestBusUpgrade(HistoricalChecksums|ValidateAppliedPrefix|EmbeddedNumbering)$' -count=1
```

Run the upgrade regressions on the central test service:

```sh
go test ./internal/store -run '^TestBusUpgrade(SchemaDefinitionsEquivalent|PreservesDataAndLedger|FailureIsAtomic|RejectsUnsafeBinaries)$' -count=1 -v
```

The numbering check must pass before release. It rejects a release that
includes 013 without 011 and 012.

Both historical and current schemas must pass. The catalog comparison
checks upgraded historical 009 and current 009 against a fresh current
install. It compares bus-message column types, nullability, and defaults;
bus-message and bus-audit index definitions; foreign keys and delete
actions; and check definitions. It ignores constraint names and deduplicates
equivalent checks.

These tests seed the schema
by running the actual migration bytes through the migrator. They preserve
message bodies, hashes, references, replies, audit metadata, identities,
devices, acceptance records, and existing presence fields. They check the
original ledger rows, new defaults, existing refusal counters, refusal
constraints, retention indexes, and the reply deletion action. They check
startup replay and direct SQL replay. A later migration writes test data
and then raises an error; the tests check schema, data, and ledger rollback.
Unsafe-binary cases must refuse before applying pending SQL. An older
migration set must refuse the upgraded database.

Run the existing migration checks and targeted bus behavior checks:

```sh
go test ./internal/store -run '^Test(Migrate|ConcurrentMigrate)' -count=1 -v
go test ./internal/bus -run '^Test(RefusalsCoalesce|RefusalsCoalescePerRecipientAndThread|RetentionDeletesFinalMessagesPastTheWindow|RetentionKeepsAClaimedMessageUntilItExpires|RetentionCapsEachSweep)$' -count=1 -v
```

These commands do not run race tests or benchmarks. The seeded fixtures
exercise the supported schema variants; they do not estimate production
DDL duration. The column changes, foreign-key validation, and index creation
take locks and run inside the startup transaction. Rehearse the integrated
release against a verified backup in an isolated environment before cutover.

## Cutover and rollback

1. Verify the integrated release includes migrations 011, 012, and 013.
2. Run the central PostgreSQL checks above.
3. Create and verify a coordinated pre-upgrade backup.
4. Stop all old server instances before the new server runs migrations.
5. Start the integrated new server under the approved cutover procedure.
6. Check startup migration results and `/healthz`.
7. Check one known provenance search and a message/reply exchange.

If startup migration is refused or its transaction fails, it commits no
pending schema changes or ledger inserts. The old build can restart without
a restore. Investigate the reported error before retrying the upgrade.
Do not edit ledger checksums to make startup succeed.

After a successful migration, rollback to 661a48b requires restoring the
matching verified pre-upgrade backup and its coordinated object storage
state. Stop the new server before the restore. Start the matching old binary
only after the restore. Restoring the backup discards writes made after it;
account for those writes in the cutover plan. Starting the old binary against
the upgraded database is refused by the ledger guard.
