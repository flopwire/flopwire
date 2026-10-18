-- Immutable sender placement captured at send time. Historical remote/main
-- cannot be proved from mutable presence or current transcript placement.
-- Do not backfill: empty snapshots explicitly mean unknown.
ALTER TABLE bus_messages ADD COLUMN from_remote text NOT NULL DEFAULT '';
ALTER TABLE bus_messages ADD COLUMN from_main text NOT NULL DEFAULT '';
-- A normal-planner fixture with 40,000 unrelated rows scanned all history
-- for a selective remote anchor. Index recorded remotes to scope thread IDs
-- before checking row authority; the existing thread/time index serves rows.
-- Include unknown remotes: the device-local fallback must also avoid scanning
-- all known-remote history on that same device.
CREATE INDEX bus_messages_remote_idx ON bus_messages (from_remote, thread_id);
