-- Only future changed flushes create work. The existing source parse queue
-- retries it, including generations superseded before a worker gets to them.
CREATE TABLE archive_redaction_work (
 source_id uuid NOT NULL,
 generation bigint NOT NULL,
 from_offset bigint NOT NULL CHECK (from_offset >= 0),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 PRIMARY KEY (source_id,generation),
 FOREIGN KEY (source_id,generation) REFERENCES generations(source_id,generation) ON DELETE CASCADE
);

-- A rewritten tail's provenance distinguishes redaction from a real file
-- rewrite when the device next sends a delta against its original prefix.
ALTER TABLE provisional_tails ADD COLUMN message_redaction_id uuid REFERENCES message_redactions(id);

-- Hash-only recognition of partly masked records; older exact-hash entries
-- remain readable without a proof. New requests record the proof atomically.
ALTER TABLE redacted_lines ADD COLUMN proof jsonb;
