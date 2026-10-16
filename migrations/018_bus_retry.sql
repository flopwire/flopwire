-- A successful explicit retry has one stable successor. Refused attempts do
-- not set retry_of and leave the source eligible for a later attempt.
-- No foreign key: retention may remove old source history independently.
ALTER TABLE bus_messages ADD COLUMN retry_of text;
CREATE UNIQUE INDEX bus_messages_retry_of_idx ON bus_messages(retry_of) WHERE retry_of IS NOT NULL;
