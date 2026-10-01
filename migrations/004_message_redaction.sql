-- Message redaction after the fact (notes/redaction.md). An owner or an
-- admin masks one message (or some of its lines), optionally with every
-- identical copy. Rows are rewritten, the archived chunks holding the
-- record are rewritten into new content-addressed chunks, and the old
-- chunks are purged by a deletion job. Nothing here holds redacted text.

CREATE TABLE message_redactions (
  id uuid PRIMARY KEY,
  requested_by uuid NOT NULL REFERENCES users (id),
  device_id uuid,
  message_id uuid NOT NULL,             -- the addressed row (no FK: rows are derived)
  lines text NOT NULL DEFAULT '',       -- 'L1-L2', or '' for the whole message
  all_copies boolean NOT NULL,
  by_admin boolean NOT NULL,
  messages integer NOT NULL,
  chunks integer NOT NULL,
  tails integer NOT NULL,
  created_at timestamptz NOT NULL
);

-- A chunk rewritten by a redaction. Ingest maps an old hash to its new one
-- in every later flush, so a device that still holds the old chunk never
-- uploads it again.
CREATE TABLE chunk_redirects (
  old_hash bytea PRIMARY KEY CHECK (octet_length(old_hash) = 32),
  new_hash bytea NOT NULL CHECK (octet_length(new_hash) = 32),
  redaction_id uuid NOT NULL REFERENCES message_redactions (id)
);

-- A redacted record line, by the SHA-256 of its archived bytes (the key
-- reveals nothing), with the spans to mask. The server's redaction pass
-- applies them at parse and raw read to any upload of the same line.
CREATE TABLE redacted_lines (
  line_sha bytea PRIMARY KEY CHECK (octet_length(line_sha) = 32),
  spans jsonb NOT NULL,
  redaction_id uuid NOT NULL REFERENCES message_redactions (id)
);

-- Counts the changes to redacted_lines. Each writing transaction bumps it
-- once per row at commit (a deferred trigger, so a writer that holds line
-- locks never waits for it mid-transaction), and a reader that sees a
-- revision sees the lines of that revision. The parse queue keeps the
-- catalog in memory and reloads it only when the revision moved.
CREATE TABLE redacted_lines_revision (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  revision bigint NOT NULL
);
INSERT INTO redacted_lines_revision (singleton, revision) VALUES (true, 0);
CREATE FUNCTION flopwire_redacted_lines_bump() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE redacted_lines_revision SET revision = revision + 1;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER redacted_lines_revision AFTER INSERT OR UPDATE OR DELETE ON redacted_lines
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION flopwire_redacted_lines_bump();
CREATE TRIGGER redacted_lines_truncate AFTER TRUNCATE ON redacted_lines
  FOR EACH STATEMENT EXECUTE FUNCTION flopwire_redacted_lines_bump();

-- The old chunks go through the existing purge worker under a job that
-- belongs to a redaction instead of a conversation tombstone.
ALTER TABLE deletion_jobs
  ALTER COLUMN tombstone_id DROP NOT NULL,
  ADD COLUMN redaction_id uuid REFERENCES message_redactions (id),
  ADD CONSTRAINT deletion_jobs_owner CHECK ((tombstone_id IS NULL) <> (redaction_id IS NULL));
