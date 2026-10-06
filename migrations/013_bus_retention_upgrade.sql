-- Forward upgrade from the authentic 661a48b bus schema to the refusal and
-- retention schema already present in current 009. Preserve all rows and the
-- historical 009 ledger checksum. This script also accepts current 009.
ALTER TABLE bus_messages
  ADD COLUMN IF NOT EXISTS attempts integer NOT NULL DEFAULT 1 CHECK (attempts >= 1),
  ADD COLUMN IF NOT EXISTS last_at timestamptz;

-- Use our own stable name: the anonymous checks in the two versions of 009
-- have different generated names. Current 009 already enforces this rule;
-- a redundant check there is harmless and avoids replacing historical checks.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'bus_messages'::regclass
      AND conname = 'bus_messages_refusal_attempts_check'
  ) THEN
    ALTER TABLE bus_messages ADD CONSTRAINT bus_messages_refusal_attempts_check
      CHECK (attempts = 1 OR state = 'refused');
  END IF;
END;
$$;

-- Only the deletion action changes. Keep existing reply links until their
-- parent is actually deleted by retention. DDL and ledger commit together.
ALTER TABLE bus_messages DROP CONSTRAINT bus_messages_reply_to_fkey;
ALTER TABLE bus_messages ADD CONSTRAINT bus_messages_reply_to_fkey
  FOREIGN KEY (reply_to) REFERENCES bus_messages (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS bus_messages_retention_idx ON bus_messages (expires_at)
  WHERE state IN ('delivered', 'read', 'expired', 'refused', 'undelivered');
CREATE INDEX IF NOT EXISTS audit_bus_message_idx ON audit_events (target_id)
  WHERE target_type = 'bus_message' AND target_id <> '';
CREATE INDEX IF NOT EXISTS audit_bus_batch_idx ON audit_events (created_at)
  WHERE target_type = 'bus_message' AND target_id = '';
