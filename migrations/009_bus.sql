-- Message bus (notes/message-bus/plan.md §4): direct messages between agent
-- sessions, presence, and per-person acceptance of cross-user senders.

-- Live sessions as each device last reported them with its long poll
-- (internal/bus). A session is live while seen_at is within
-- busproto.PresenceTTL. Every poll replaces its device's rows. user_id is
-- the owner; device_id is NULL-able so a session owned by a user and no
-- device (a vendor cloud session) can be added later.
CREATE TABLE bus_presence (
  device_id uuid REFERENCES devices (id),
  user_id uuid NOT NULL REFERENCES users (id),
  agent text NOT NULL,
  session_id text NOT NULL,
  -- repo is the repo root as the device placed the session; names compare
  -- by its last path element.
  repo text NOT NULL DEFAULT '',
  branch text NOT NULL DEFAULT '',
  title text NOT NULL DEFAULT '',
  busy boolean NOT NULL,
  seen_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX bus_presence_device_session_idx ON bus_presence (device_id, agent, session_id);
-- Recipient prefixes, sender checks and claims look a session up by id.
CREATE INDEX bus_presence_session_idx ON bus_presence ((session_id COLLATE "C"));
-- peers and the @user eligibility check: the live set.
CREATE INDEX bus_presence_seen_idx ON bus_presence (seen_at);
CREATE INDEX bus_presence_user_idx ON bus_presence (user_id, seen_at);

-- seq orders a recipient's deliverable messages for the long poll; it is
-- reassigned when a held message is released, so the release wakes the poll.
CREATE SEQUENCE bus_messages_seq;

-- One message. The envelope (from_*, sender, intent, thread_id, created_at)
-- is set by the server from the sender's device credential; the client
-- supplies only the recipient, body, intent, reply_to, refs and repo.
--
-- to_user is always the recipient person. A message to a session has
-- to_session and to_agent from the start; a message to @user has them NULL
-- until a device claims it for one live session (claimed_by, the same
-- session id) and is routed by to_repo, a repo name ('' for any).
--
-- state: queued (deliverable), held (B7: a cross-user sender the recipient
-- has not accepted), claimed (an @user message one device took), delivered
-- (delivered_at: a hook printed it and confirmed the print), read (read_at:
-- the recipient session's transcript then showed it in hook context, as a
-- read receipt from the device holding that session reported; first
-- receipt wins), expired (undelivered at expires_at), refused (a send limit;
-- reason), undelivered (the device gave up on it; reason: unconfirmed, no
-- hook confirmed printing it after devicebus.MaxAttempts leases).
CREATE TABLE bus_messages (
  id text PRIMARY KEY,
  seq bigint NOT NULL DEFAULT nextval('bus_messages_seq'),
  thread_id text NOT NULL,
  reply_to text REFERENCES bus_messages (id),
  from_user uuid NOT NULL REFERENCES users (id),
  from_device uuid REFERENCES devices (id),
  from_agent text NOT NULL,
  from_session text NOT NULL,
  from_repo text NOT NULL DEFAULT '',
  from_branch text NOT NULL DEFAULT '',
  to_user uuid NOT NULL REFERENCES users (id),
  to_agent text,
  to_session text,
  to_repo text NOT NULL DEFAULT '',
  addressed text NOT NULL CHECK (addressed IN ('session', 'user')),
  sender text NOT NULL CHECK (sender IN ('own', 'teammate')),
  intent text NOT NULL CHECK (intent IN ('request', 'inform', 'done')),
  -- body is redacted by the server before it is stored; at most
  -- busproto.MaxBodyBytes.
  body text NOT NULL,
  body_sha bytea NOT NULL CHECK (octet_length(body_sha) = 32),
  refs text[] NOT NULL DEFAULT '{}',
  state text NOT NULL
    CHECK (state IN ('queued', 'held', 'claimed', 'delivered', 'read', 'expired', 'refused', 'undelivered')),
  reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  claimed_by text,
  claimed_device uuid REFERENCES devices (id),
  claimed_at timestamptz,
  delivered_at timestamptz,
  read_at timestamptz,
  CHECK (addressed = 'user' OR to_session IS NOT NULL),
  CHECK (addressed = 'session' OR (to_session IS NULL) = (claimed_by IS NULL)),
  CHECK ((to_session IS NULL) = (to_agent IS NULL))
);
-- The long poll: a recipient's deliverable messages in seq order.
CREATE INDEX bus_messages_deliver_idx ON bus_messages (to_user, seq)
  WHERE state IN ('queued', 'claimed');
-- Held messages per recipient (accept releases them, the held summary
-- counts them) and the per-recipient undelivered limit for @user messages.
CREATE INDEX bus_messages_pending_user_idx ON bus_messages (to_user, from_user)
  WHERE state IN ('queued', 'held', 'claimed');
-- A session's inbox and the per-session undelivered limit.
CREATE INDEX bus_messages_to_session_idx ON bus_messages (to_session, created_at)
  WHERE to_session IS NOT NULL;
-- A session's sent messages, its hourly send limit and the duplicate check.
CREATE INDEX bus_messages_from_session_idx ON bus_messages (from_session, created_at);
-- The per-device and per-person hourly send ceilings.
CREATE INDEX bus_messages_from_device_idx ON bus_messages (from_device, created_at);
CREATE INDEX bus_messages_from_user_idx ON bus_messages (from_user, created_at);
-- The per-thread hourly limit and a thread's messages.
CREATE INDEX bus_messages_thread_idx ON bus_messages (thread_id, created_at);
CREATE INDEX bus_messages_reply_to_idx ON bus_messages (reply_to) WHERE reply_to IS NOT NULL;
-- The expiry sweep.
CREATE INDEX bus_messages_expiry_idx ON bus_messages (expires_at)
  WHERE state IN ('queued', 'held', 'claimed');

-- B7: recipient_user accepts messages from sender_user. Revoking deletes
-- the row.
CREATE TABLE bus_accepts (
  recipient_user uuid NOT NULL REFERENCES users (id),
  sender_user uuid NOT NULL REFERENCES users (id),
  created_at timestamptz NOT NULL,
  PRIMARY KEY (recipient_user, sender_user),
  CHECK (recipient_user <> sender_user)
);
