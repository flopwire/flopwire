-- Flopwire schema, fresh start (spec notes/local-search/README.md §4, §7, §10).
--
-- Every earlier migration (the CASS-era segments model) is gone. A database
-- created by those migrations is refused by the runner; see
-- internal/store/migrate.go.
--
-- Backup contract (§7.2, §10): identity, audit, policy, deletion, sources,
-- generations, chunks, manifest_entries, provisional_tails, plus the S3
-- bucket. conversations and messages are derived and rebuildable by replaying
-- manifests (and tails) through the parser. pg_dump takes all of it anyway.

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------

CREATE TABLE users (
  id uuid PRIMARY KEY,
  email text NOT NULL,
  name text NOT NULL,
  role text NOT NULL CHECK (role IN ('admin', 'member')),
  identity_type text NOT NULL CHECK (identity_type IN ('human', 'service')),
  password_hash text NOT NULL DEFAULT '',
  disabled boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX users_email_unique_idx ON users (lower(email));

CREATE TABLE invites (
  id uuid PRIMARY KEY,
  email text NOT NULL,
  role text NOT NULL CHECK (role IN ('admin', 'member')),
  code_hash text NOT NULL UNIQUE,
  created_by uuid NOT NULL REFERENCES users (id),
  expires_at timestamptz NOT NULL,
  claimed_at timestamptz
);

-- kind: 'device' is an enrolled laptop (rotating credential, 90-day
-- re-login, 30-day idle limit); 'ephemeral' registers itself on the first
-- use of a minted token (a sandbox, a CI job); 'service' is a service
-- identity's upload device. last_seen_at and last_ip are refreshed on
-- authenticated requests. swept_at hides an expired ephemeral device from
-- the device list; the row stays, so its uploads and audit records keep
-- their device.
CREATE TABLE devices (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users (id),
  name text NOT NULL,
  platform text NOT NULL,
  kind text NOT NULL DEFAULT 'device' CHECK (kind IN ('device', 'ephemeral', 'service')),
  label text NOT NULL DEFAULT '',
  last_seen_at timestamptz,
  last_ip text NOT NULL DEFAULT '',
  revoked_at timestamptz,
  swept_at timestamptz,
  created_at timestamptz NOT NULL
);
CREATE INDEX devices_user_idx ON devices (user_id);

-- active=false marks a device credential that a prepared rotation issued but
-- has not yet committed; it authenticates only the rotation commit call.
--
-- kind 'minted' is a short-lived scoped token (`flopwire token mint`) bound to
-- the minting user. Its ephemeral device is created on first use, so
-- device_id starts NULL. minted_from_device is the device whose credential
-- minted it (NULL when a login session did): revoking that device revokes
-- the token too. scopes lists 'upload' and 'read'; empty means the kind's
-- default (a human device: both; a service device: upload; a session: read).
-- revoke_reason says why revoked_at is set: rotated, revoked, reauth,
-- expired, idle or principal_revoked.
CREATE TABLE credentials (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users (id),
  device_id uuid REFERENCES devices (id),
  kind text NOT NULL CHECK (kind IN ('session', 'device', 'minted')),
  token_hash text NOT NULL UNIQUE,
  active boolean NOT NULL DEFAULT true,
  scopes text[] NOT NULL DEFAULT '{}' CHECK (scopes <@ ARRAY['upload', 'read']::text[]),
  label text NOT NULL DEFAULT '',
  minted_from_device uuid REFERENCES devices (id),
  expires_at timestamptz,
  revoked_at timestamptz,
  revoke_reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL,
  CHECK (kind <> 'session' OR device_id IS NULL),
  CHECK (kind <> 'device' OR device_id IS NOT NULL),
  CHECK (kind <> 'minted' OR (expires_at IS NOT NULL AND cardinality(scopes) > 0))
);
CREATE INDEX credentials_device_idx ON credentials (device_id) WHERE device_id IS NOT NULL;
CREATE INDEX credentials_user_idx ON credentials (user_id);
CREATE INDEX credentials_minted_from_idx ON credentials (minted_from_device) WHERE minted_from_device IS NOT NULL;
-- The sweeper's candidates: live credentials that can expire.
CREATE INDEX credentials_live_expiry_idx ON credentials (expires_at) WHERE revoked_at IS NULL AND kind <> 'session';

CREATE TABLE device_rotations (
  id uuid PRIMARY KEY,
  device_id uuid NOT NULL REFERENCES devices (id),
  old_credential_id uuid NOT NULL REFERENCES credentials (id),
  new_credential_id uuid NOT NULL UNIQUE REFERENCES credentials (id),
  commit_token_hash text NOT NULL UNIQUE,
  state text NOT NULL CHECK (state IN ('prepared', 'committed')),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL,
  committed_at timestamptz
);
CREATE UNIQUE INDEX device_rotations_one_prepared_idx
  ON device_rotations (device_id) WHERE state = 'prepared';

-- ---------------------------------------------------------------------------
-- Audit and policy
-- ---------------------------------------------------------------------------

CREATE TABLE audit_events (
  id uuid PRIMARY KEY,
  actor_id uuid REFERENCES users (id),
  device_id uuid REFERENCES devices (id),
  action text NOT NULL,
  target_type text NOT NULL DEFAULT '',
  target_id text NOT NULL DEFAULT '',
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL
);
CREATE INDEX audit_created_idx ON audit_events (created_at DESC);
CREATE INDEX audit_actor_idx ON audit_events (actor_id, created_at DESC);

CREATE TABLE collection_policy (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  max_storage_bytes bigint NOT NULL DEFAULT 0 CHECK (max_storage_bytes >= 0),
  max_user_bytes bigint NOT NULL DEFAULT 0 CHECK (max_user_bytes >= 0),
  path_rules jsonb NOT NULL DEFAULT '[]'::jsonb,
  unplaceable text NOT NULL DEFAULT '' CHECK (unplaceable IN ('', 'local', 'upload', 'exclude')),
  -- The longest TTL `flopwire token mint` may ask for; 0 means the default
  -- (24 hours).
  max_token_ttl_seconds bigint NOT NULL DEFAULT 0 CHECK (max_token_ttl_seconds >= 0),
  updated_by uuid REFERENCES users (id),
  updated_at timestamptz
);
INSERT INTO collection_policy (singleton) VALUES (true);

-- ---------------------------------------------------------------------------
-- Raw evidence: sources, generations, chunks, manifests, provisional tails
-- (§4.1). S3 holds only finalized, content-addressed chunks.
-- ---------------------------------------------------------------------------

CREATE TABLE sources (
  id uuid PRIMARY KEY,
  device_id uuid NOT NULL REFERENCES devices (id),
  agent text NOT NULL,
  path text NOT NULL,
  file_id text NOT NULL,
  session_key text,
  storage_kind text NOT NULL
    CHECK (storage_kind IN ('jsonl_append', 'json_doc', 'sqlite', 'dir', 'markdown')),
  parser text NOT NULL,
  first_seen_at timestamptz NOT NULL,
  UNIQUE (device_id, path, file_id)
);

CREATE TABLE generations (
  source_id uuid NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
  generation bigint NOT NULL CHECK (generation >= 0),
  size bigint NOT NULL CHECK (size >= 0),
  change_time timestamptz,
  captured_at timestamptz NOT NULL,
  complete boolean NOT NULL,
  PRIMARY KEY (source_id, generation)
);

-- One row per finalized chunk object. The state columns are the raw-object
-- ledger (salvaged from the CASS-era raw_object_ledger, remapped to chunks):
--
--   uploading        reserved before the S3 put; becomes committed with the
--                    manifest transaction that first references it
--   committed        present in S3 and safe to reference
--   cleanup_pending  an upload that never committed; the orphan reconciler
--   purging          owns it and deletes the object once cleanup_after passes
--   deletion_pending the deletion job that dropped its last manifest reference
--   purging_delete   owns it (deletion_job_id) and deletes the object
--
-- A purged chunk's row is removed after S3 confirms the delete. Content
-- addressing means a later upload of the same bytes simply reserves it again.
-- manifest_entries.chunk_hash has no ON DELETE action, so a chunk row that is
-- still referenced cannot be removed.
CREATE TABLE chunks (
  hash bytea PRIMARY KEY CHECK (octet_length(hash) = 32),
  size integer NOT NULL CHECK (size > 0), -- uncompressed bytes
  -- The object is the chunk compressed (encoding); stored_size is its
  -- length in object storage. The hash is of the uncompressed bytes.
  stored_size integer NOT NULL CHECK (stored_size > 0),
  encoding text NOT NULL DEFAULT 'zstd' CHECK (encoding = 'zstd'),
  object_key text NOT NULL UNIQUE,
  state text NOT NULL DEFAULT 'committed'
    CHECK (state IN ('uploading', 'committed', 'cleanup_pending', 'purging',
                     'deletion_pending', 'purging_delete')),
  deletion_job_id uuid,
  uploaded_by_device uuid REFERENCES devices (id),
  cleanup_after timestamptz,
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  last_error_class text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((state IN ('deletion_pending', 'purging_delete')) = (deletion_job_id IS NOT NULL))
);
CREATE INDEX chunks_orphan_idx ON chunks (cleanup_after, created_at)
  WHERE state IN ('uploading', 'cleanup_pending', 'purging');
CREATE INDEX chunks_deletion_job_idx ON chunks (deletion_job_id)
  WHERE deletion_job_id IS NOT NULL;

CREATE TABLE manifest_entries (
  source_id uuid NOT NULL,
  generation bigint NOT NULL,
  ordinal integer NOT NULL CHECK (ordinal >= 0),
  chunk_hash bytea NOT NULL REFERENCES chunks (hash),
  byte_offset bigint NOT NULL CHECK (byte_offset >= 0),
  PRIMARY KEY (source_id, generation, ordinal),
  FOREIGN KEY (source_id, generation)
    REFERENCES generations (source_id, generation) ON DELETE CASCADE
);
CREATE INDEX manifest_entries_chunk_idx ON manifest_entries (chunk_hash);

CREATE TABLE provisional_tails (
  source_id uuid NOT NULL,
  generation bigint NOT NULL,
  byte_offset bigint NOT NULL CHECK (byte_offset >= 0),
  bytes bytea NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (source_id, generation),
  FOREIGN KEY (source_id, generation)
    REFERENCES generations (source_id, generation) ON DELETE CASCADE
);

-- ---------------------------------------------------------------------------
-- Derived rows: conversations and messages (§4.2, §4.3)
-- ---------------------------------------------------------------------------

CREATE TABLE conversations (
  id uuid PRIMARY KEY,
  source_id uuid REFERENCES sources (id) ON DELETE SET NULL,
  agent text NOT NULL,
  session_id text NOT NULL,
  device_id uuid NOT NULL REFERENCES devices (id),
  user_id uuid NOT NULL REFERENCES users (id),
  cwd text,
  repo_root text,
  title text,
  started_at timestamptz,
  last_activity_at timestamptz,
  -- Subagent linkage. The native ids are kept so a child that arrives before
  -- its parent resolves parent_conversation_id when the parent lands.
  parent_conversation_id uuid REFERENCES conversations (id) ON DELETE SET NULL,
  spawned_by_message_id uuid,
  parent_native_session_id text,
  spawned_by_native_id text,
  depth smallint NOT NULL DEFAULT 0 CHECK (depth >= 0),
  extra jsonb NOT NULL DEFAULT '{}'::jsonb,
  -- The git branches the session ran on, first seen first.
  branches text[] NOT NULL DEFAULT '{}',
  -- The conversation's digest (internal/digest), refreshed on every append.
  digest jsonb,
  -- The digest's counts wait for a recount: a parse replaced rows and
  -- has not completed (ingest refreshDigest, digestFold).
  digest_stale boolean NOT NULL DEFAULT false,
  UNIQUE (device_id, agent, session_id)
);
CREATE INDEX conversations_user_idx ON conversations (user_id, last_activity_at DESC);
CREATE INDEX conversations_repo_idx ON conversations (repo_root) WHERE repo_root IS NOT NULL;
-- Retrieval addresses name a session by any unique prefix of its id.
CREATE INDEX conversations_session_idx ON conversations ((session_id COLLATE "C"));
CREATE INDEX conversations_parent_idx ON conversations (parent_conversation_id)
  WHERE parent_conversation_id IS NOT NULL;
CREATE INDEX conversations_unresolved_parent_idx ON conversations (device_id, agent, parent_native_session_id)
  WHERE parent_native_session_id IS NOT NULL AND parent_conversation_id IS NULL;
-- A conversation's subagents by native session, resolved or not: the
-- digest's subagent count, refreshed on every parse batch.
CREATE INDEX conversations_subagents_idx ON conversations (device_id, agent, parent_native_session_id)
  WHERE parent_native_session_id IS NOT NULL;

-- text is the message's extracted text, whole and plain (no cap): the one
-- copy that rendering, `find` (pg_trgm over the full text) and ranked
-- search (tsv) all read. TOAST compresses it with lz4, and the low
-- toast_tuple_target moves long texts out of line early, so heap pages hold
-- mostly row metadata and bitmap scans stay cheap. tsv covers the first
-- 200,000 characters: a tsvector's lexemes must fit in 1MB, and 200k
-- characters of 4-byte UTF-8 stay under that.
CREATE TABLE messages (
  id uuid PRIMARY KEY,
  conversation_id uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
  source_id uuid REFERENCES sources (id) ON DELETE SET NULL,
  native_id text,
  parent_native_id text,
  part integer NOT NULL DEFAULT 0 CHECK (part >= 0),
  ordinal bigint NOT NULL,
  kind text NOT NULL
    CHECK (kind IN ('unknown', 'user', 'assistant', 'tool_call', 'tool_result', 'thinking', 'system',
                    'injected', 'agent_message')),
  role text,
  tool_name text,
  tool_call_id text,
  is_error boolean,
  ts timestamptz,
  text text NOT NULL,
  text_len integer NOT NULL CHECK (text_len >= 0),
  content_sha bytea NOT NULL CHECK (octet_length(content_sha) = 32),
  version integer NOT NULL DEFAULT 1 CHECK (version >= 1),
  superseded boolean NOT NULL DEFAULT false,
  -- Deferred: the update that supersedes a row names the new version,
  -- which the same transaction inserts after it.
  superseded_by uuid REFERENCES messages (id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED,
  superseded_in_generation bigint,
  on_active_path boolean,
  enrichment jsonb,
  source_generation bigint NOT NULL,
  line_no bigint,
  byte_offset bigint,
  byte_len bigint,
  locator text,
  parser text NOT NULL,
  tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, left(text, 200000))) STORED
) WITH (toast_tuple_target = 512);
ALTER TABLE messages ALTER COLUMN text SET COMPRESSION lz4;

CREATE INDEX messages_tsv_idx ON messages USING gin (tsv);
CREATE INDEX messages_conversation_ordinal_idx ON messages (conversation_id, ordinal);
CREATE INDEX messages_native_idx ON messages (native_id) WHERE native_id IS NOT NULL;
-- Rows with the same text: a message redaction's copies (all_copies, and
-- byte-identical records in other sources), probed while it holds the
-- redacted-lines lock that every flush and parse write waits for.
CREATE INDEX messages_content_sha_idx ON messages (content_sha);
CREATE INDEX messages_default_filter_idx ON messages (conversation_id, ordinal)
  WHERE NOT superseded AND on_active_path IS NOT FALSE;
-- Failed tool calls, for the digest's count on append.
CREATE INDEX messages_failed_idx ON messages (conversation_id, tool_call_id) WHERE is_error;
-- One live row per native record part. Superseded versions stay alongside it.
CREATE UNIQUE INDEX messages_live_native_idx ON messages (conversation_id, native_id, part)
  WHERE native_id IS NOT NULL AND NOT superseded;

-- pg_trgm for `find` (substring and regex prefilter). The extension is
-- database-wide, so it may already live in another schema; resolve its
-- operator class wherever it is.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
DO $$
DECLARE trgm_schema text;
BEGIN
  SELECT n.nspname INTO STRICT trgm_schema
    FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
   WHERE e.extname = 'pg_trgm';
  EXECUTE format('CREATE INDEX messages_text_trgm_idx ON messages USING gin (text %I.gin_trgm_ops)', trgm_schema);
  -- The sessions glob (retrieval sessionsPage) matches session id, title
  -- and repo (or cwd) with ILIKE '%...%': each column's trigrams select a
  -- rare glob's few sessions instead of the list being walked and
  -- filtered. The expressions are the predicate's own.
  EXECUTE format('CREATE INDEX conversations_session_trgm_idx ON conversations USING gin (session_id %I.gin_trgm_ops)', trgm_schema);
  EXECUTE format('CREATE INDEX conversations_title_trgm_idx ON conversations USING gin ((COALESCE(title, '''')) %I.gin_trgm_ops)', trgm_schema);
  EXECUTE format('CREATE INDEX conversations_place_trgm_idx ON conversations USING gin ((COALESCE(repo_root, cwd, '''')) %I.gin_trgm_ops)', trgm_schema);
END
$$;

-- ---------------------------------------------------------------------------
-- Deletion (§10): tombstones and durable purge jobs
-- ---------------------------------------------------------------------------

-- A tombstone names a deleted session by its natural key per user, so the
-- same session from any of the user's devices, uploaded again or for the
-- first time, never comes back (spec §10, D9); ingest checks this table.
-- Deleting a conversation tombstones its session and every subagent
-- session below it; job_id is the deletion job that purges them (the
-- root's), and device_id the device whose conversation was named.
CREATE TABLE conversation_tombstones (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users (id),
  device_id uuid NOT NULL REFERENCES devices (id),
  agent text NOT NULL,
  session_id text NOT NULL,
  conversation_id uuid NOT NULL,
  job_id uuid,
  requested_by uuid NOT NULL REFERENCES users (id),
  requested_at timestamptz NOT NULL,
  UNIQUE (user_id, agent, session_id),
  UNIQUE (conversation_id)
);

-- The job purges raw evidence after the request transaction has already
-- removed the conversation's rows: the sources (with their generations,
-- manifests, and tails) that no remaining conversation or message still
-- references, then every chunk left without a manifest reference.
-- source_ids records the candidates, since the rows that named them are gone.
CREATE TABLE deletion_jobs (
  id uuid PRIMARY KEY,
  tombstone_id uuid NOT NULL UNIQUE REFERENCES conversation_tombstones (id),
  source_ids uuid[] NOT NULL DEFAULT '{}',
  requested_by uuid NOT NULL REFERENCES users (id),
  state text NOT NULL CHECK (state IN ('queued', 'purging', 'retry_wait', 'complete', 'failed')),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_attempt_at timestamptz,
  last_error text NOT NULL DEFAULT '',
  locked_at timestamptz,
  requested_at timestamptz NOT NULL,
  completed_at timestamptz
);
CREATE INDEX deletion_jobs_work_idx ON deletion_jobs (state, next_attempt_at, requested_at);

ALTER TABLE chunks
  ADD CONSTRAINT chunks_deletion_job_fk FOREIGN KEY (deletion_job_id) REFERENCES deletion_jobs (id);
ALTER TABLE conversation_tombstones
  ADD CONSTRAINT conversation_tombstones_job_fk FOREIGN KEY (job_id) REFERENCES deletion_jobs (id);
