-- Historical schema from 6ca4fdc parent, before the index-only version 5 bump.

CREATE TABLE IF NOT EXISTS devbus_messages (
  id            TEXT PRIMARY KEY,
  origin        TEXT NOT NULL,              -- server or local
  seq           INTEGER NOT NULL,           -- delivery order (the server's seq, or local)
  to_session    TEXT NOT NULL,              -- '' while a local @user message is unclaimed
  to_agent      TEXT NOT NULL,
  from_session  TEXT NOT NULL,
  from_agent    TEXT NOT NULL,
  thread_id     TEXT NOT NULL,
  to_key        TEXT NOT NULL DEFAULT '',   -- local: the recipient, for the limits
  body_sha      BLOB,                       -- local: the duplicate check
  envelope      TEXT NOT NULL,              -- busproto.Envelope JSON
  -- queued, leased (a hook took it and has not confirmed printing it),
  -- delivered (confirmed), undelivered (MaxAttempts leases, none
  -- confirmed; or its session ended first), refused (local)
  state         TEXT NOT NULL,
  reason        TEXT NOT NULL DEFAULT '',   -- refused: the limit's code; undelivered: busproto.ReasonUnconfirmed or ReasonSessionEnded
  attempts      INTEGER NOT NULL DEFAULT 0, -- leases handed to hooks
  lease_until   INTEGER,                    -- leased: unix ms when the lease ends
  created_at    INTEGER NOT NULL,           -- unix ms
  expires_at    INTEGER NOT NULL,
  delivered_at  INTEGER,
  -- server: '' none owed, owed (a delivery receipt), report (an undelivered
  -- report), done, rejected
  ack           TEXT NOT NULL DEFAULT '',
  listed        INTEGER NOT NULL DEFAULT 1, -- server: in the last poll's set
  -- read_at: the first time the recipient session's transcript showed the
  -- message in hook context (unix ms; markRead), kept from a lease on and
  -- shown once the message is delivered. read_ack, server: '' none owed,
  -- owed (a read receipt), done, rejected.
  read_at       INTEGER,
  read_ack      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS devbus_to ON devbus_messages (to_session, state);
CREATE INDEX IF NOT EXISTS devbus_ack ON devbus_messages (ack) WHERE ack IN ('owed', 'report');
CREATE INDEX IF NOT EXISTS devbus_read_ack ON devbus_messages (read_ack, read_at, id) WHERE read_ack = 'owed';
CREATE INDEX IF NOT EXISTS devbus_lease ON devbus_messages (lease_until) WHERE state = 'leased';
CREATE INDEX IF NOT EXISTS devbus_from ON devbus_messages (from_session, created_at);
CREATE INDEX IF NOT EXISTS devbus_thread ON devbus_messages (thread_id, created_at);
CREATE INDEX IF NOT EXISTS devbus_expires ON devbus_messages (expires_at);
-- The device's sessions as the bus tracks them (Bus.Observe, End,
-- Revive): whether a harness registry holds each open, when it was last
-- live, and whether and when it ended (#67, #82).
CREATE TABLE IF NOT EXISTS devbus_sessions (
  agent         TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  holder        TEXT NOT NULL DEFAULT '',   -- the process a registry names as holding it open ('' none)
  held_at       INTEGER,                    -- last seen held (unix ms)
  missing_since INTEGER,                    -- held before, and its registry entry missing since
  live_at       INTEGER,                    -- last seen live in presence
  ended_at      INTEGER,                    -- ended: unix ms; NULL while not ended
  ended_by      TEXT NOT NULL DEFAULT '',   -- hook; registry once its registry showed it gone (any process holding it later is a resume)
  ended_holder  TEXT NOT NULL DEFAULT '',   -- the holder when it ended, while it still holds it
  PRIMARY KEY (agent, session_id)
);
CREATE INDEX IF NOT EXISTS devbus_sessions_id ON devbus_sessions (session_id);
-- The standing instruction of each session, delivered like a message
-- (#101): leased to a hook, owed until one confirms printing it.
CREATE TABLE IF NOT EXISTS devbus_instruct (
  session_id    TEXT PRIMARY KEY,
  confirmed_at  INTEGER,                    -- a hook confirmed printing it
  lease_until   INTEGER,                    -- leased to a hook until then
  attempts      INTEGER NOT NULL DEFAULT 0, -- leases since it was last owed
  updated_at    INTEGER NOT NULL
);
-- The held-message notice (Bus.HeldNotice): when the user was last told
-- about each sender's held messages.
CREATE TABLE IF NOT EXISTS devbus_notices (
  sender_user TEXT PRIMARY KEY,
  noticed_at  INTEGER NOT NULL              -- unix ms
);

PRAGMA user_version=4;
