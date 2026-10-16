-- A successful explicit retry has one stable successor. Refused attempts do
-- not set retry_of and leave the source eligible for a later attempt.
-- No foreign key: retention may remove old source history independently.
ALTER TABLE bus_messages ADD COLUMN retry_of text;
CREATE UNIQUE INDEX bus_messages_retry_of_idx ON bus_messages(retry_of) WHERE retry_of IS NOT NULL;

-- The admission boundary is immutable even when recipient presence changes.
ALTER TABLE bus_messages ADD COLUMN admission text NOT NULL DEFAULT ''
  CHECK (admission IN ('', 'next_resume'));

-- Keep the receipt after successor deletion while the source still exists.
-- Neither reference cascades: deleting either message cannot enable a new send.
-- Sweep removes the receipt only after its source message no longer exists.
CREATE TABLE bus_retries (
  source_id text PRIMARY KEY,
  successor_id text NOT NULL UNIQUE
);
INSERT INTO bus_retries(source_id,successor_id)
  SELECT retry_of,id FROM bus_messages WHERE retry_of IS NOT NULL;
