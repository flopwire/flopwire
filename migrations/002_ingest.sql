-- Server ingest (spec §7.1, B3).

-- Companion files (Claude tool-results/*, agent-*.meta.json) are sources of
-- their own, archived whole and read by their parent's parser.
ALTER TABLE sources DROP CONSTRAINT sources_storage_kind_check;
ALTER TABLE sources ADD CONSTRAINT sources_storage_kind_check
  CHECK (storage_kind IN ('jsonl_append', 'json_doc', 'sqlite', 'dir', 'markdown', 'companion'));

-- parent_source_id links a companion to its transcript. The device names the
-- parent by (path, file_id); an empty file_id resolves by path alone. A
-- companion that arrives before its parent keeps parent_path and resolves
-- when the parent lands.
--
-- previous_source_id names the source this one replaced (a new file
-- identity at the same path, or a file moved from another path, D20); its
-- rows are superseded once this one is parsed. previous_path and
-- previous_file_id keep the device's reference until that source arrives,
-- so a link to a source uploaded later is still made.
--
-- tombstoned_at marks a source whose conversation was deleted: its raw
-- evidence is gone and later uploads to it are acknowledged and discarded,
-- so a re-upload never resurrects the conversation (§10).
--
-- refused_rule, set with tombstoned_at, is the admin path rule (or
-- "unplaceable=...") that made the server refuse the source at parse time
-- or purge it after a rule change (D18); the flush answer names it so the
-- device can report it.
ALTER TABLE sources
  ADD COLUMN parent_source_id uuid REFERENCES sources (id) ON DELETE SET NULL,
  ADD COLUMN parent_path text,
  ADD COLUMN parent_file_id text,
  ADD COLUMN previous_source_id uuid REFERENCES sources (id) ON DELETE SET NULL,
  ADD COLUMN previous_path text,
  ADD COLUMN previous_file_id text,
  ADD COLUMN tombstoned_at timestamptz,
  ADD COLUMN refused_rule text;
CREATE INDEX sources_parent_idx ON sources (parent_source_id) WHERE parent_source_id IS NOT NULL;
CREATE INDEX sources_unresolved_previous_idx ON sources (device_id, previous_path)
  WHERE previous_path IS NOT NULL AND previous_source_id IS NULL;
CREATE INDEX sources_unresolved_parent_idx ON sources (device_id, parent_path)
  WHERE parent_path IS NOT NULL AND parent_source_id IS NULL;

-- Parser progress per source: derived, like message rows, and rebuilt by
-- resetting it. The durable parse queue is requested_seq > parsed_seq: a
-- flush bumps requested_seq in its manifest transaction, so work survives a
-- restart; a parse clears it only if no flush arrived meanwhile.
CREATE TABLE source_parse_state (
  source_id uuid PRIMARY KEY REFERENCES sources (id) ON DELETE CASCADE,
  generation bigint NOT NULL DEFAULT -1,  -- generation the cursor belongs to
  cursor_offset bigint NOT NULL DEFAULT 0,
  cursor_line bigint NOT NULL DEFAULT 0,
  cursor_state bytea,
  requested_seq bigint NOT NULL DEFAULT 1,
  parsed_seq bigint NOT NULL DEFAULT 0,
  reparse boolean NOT NULL DEFAULT false,  -- parse the generation again from its start
  attempts integer NOT NULL DEFAULT 0,     -- consecutive failures; a flush does not reset them
  next_attempt_at timestamptz,
  last_error text NOT NULL DEFAULT '',
  quarantined_at timestamptz,               -- failed too often; not retried until released
  requested_at timestamptz NOT NULL DEFAULT now(),
  parsed_at timestamptz
);
CREATE INDEX source_parse_pending_idx ON source_parse_state (next_attempt_at NULLS FIRST, requested_at)
  WHERE requested_seq > parsed_seq AND quarantined_at IS NULL;

-- Row identity for records without a native id: (source, locator, part).
CREATE INDEX messages_source_locator_idx ON messages (source_id, locator, part)
  WHERE native_id IS NULL AND NOT superseded;
-- Every row per source, superseded ones included: deletion and refusal
-- lookups, and ON DELETE SET NULL from sources. superseded second also
-- serves live rows per source, for supersession when a generation drops
-- them.
CREATE INDEX messages_source_idx ON messages (source_id, superseded);
-- ON DELETE SET NULL from a deleted message to the versions it superseded.
CREATE INDEX messages_superseded_by_idx ON messages (superseded_by) WHERE superseded_by IS NOT NULL;
-- Conversations per source: deletion lookups and ON DELETE SET NULL from sources.
CREATE INDEX conversations_source_idx ON conversations (source_id);

-- Stored bytes per user (a chunk's compressed object, stored_size; a
-- tail's bytes), for quotas and admin status (V5): kept by
-- triggers on every chunk row and provisional tail, so a flush checks its
-- quota with two small reads instead of summing every chunk under a
-- global lock. A chunk counts once, against the user whose device
-- reserved it (uploaded_by_device); a tail against its source's user.
-- owner '' holds bytes no device is named for.
CREATE TABLE storage_usage (
  owner text PRIMARY KEY,
  bytes bigint NOT NULL DEFAULT 0
);

CREATE FUNCTION flopwire_usage_add(dev uuid, delta bigint) RETURNS void LANGUAGE sql AS $$
  INSERT INTO storage_usage (owner, bytes)
  VALUES (COALESCE((SELECT user_id::text FROM devices WHERE id = dev), ''), delta)
  ON CONFLICT (owner) DO UPDATE SET bytes = storage_usage.bytes + excluded.bytes
$$;

CREATE FUNCTION flopwire_chunk_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP IN ('UPDATE', 'DELETE') THEN
    PERFORM flopwire_usage_add(OLD.uploaded_by_device, -OLD.stored_size);
  END IF;
  IF TG_OP IN ('INSERT', 'UPDATE') THEN
    PERFORM flopwire_usage_add(NEW.uploaded_by_device, NEW.stored_size);
  END IF;
  RETURN NULL;
END
$$;
CREATE TRIGGER chunks_usage_insert_delete AFTER INSERT OR DELETE ON chunks
  FOR EACH ROW EXECUTE FUNCTION flopwire_chunk_usage();
CREATE TRIGGER chunks_usage_update AFTER UPDATE OF uploaded_by_device, stored_size ON chunks
  FOR EACH ROW WHEN (OLD.uploaded_by_device IS DISTINCT FROM NEW.uploaded_by_device OR OLD.stored_size <> NEW.stored_size)
  EXECUTE FUNCTION flopwire_chunk_usage();

CREATE FUNCTION flopwire_tail_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP IN ('UPDATE', 'DELETE') THEN
    PERFORM flopwire_usage_add((SELECT device_id FROM sources WHERE id = OLD.source_id), -octet_length(OLD.bytes));
  END IF;
  IF TG_OP IN ('INSERT', 'UPDATE') THEN
    PERFORM flopwire_usage_add((SELECT device_id FROM sources WHERE id = NEW.source_id), octet_length(NEW.bytes));
  END IF;
  RETURN NULL;
END
$$;
CREATE TRIGGER provisional_tails_usage AFTER INSERT OR UPDATE OR DELETE ON provisional_tails
  FOR EACH ROW EXECUTE FUNCTION flopwire_tail_usage();

-- Admin path rules are enforced on the server too (D18). rules_version
-- counts policy updates; the parse queue re-checks stored sessions against
-- the rules until rules_swept_version catches up.
ALTER TABLE collection_policy
  ADD COLUMN rules_version bigint NOT NULL DEFAULT 0,
  ADD COLUMN rules_swept_version bigint NOT NULL DEFAULT 0;

-- A stored conversation that a rule change covers is hidden, not deleted
-- (D18): retrieval leaves it out, and it is purged when an administrator
-- confirms or once it has been hidden for seven days; a rule change that
-- no longer covers it restores it first. hidden_root is the conversation
-- the rule matched; its subagents and the same session on the user's
-- other devices are hidden with it and name it.
ALTER TABLE conversations
  ADD COLUMN hidden_at timestamptz,
  ADD COLUMN hidden_rule text,
  ADD COLUMN hidden_rules_version bigint,
  ADD COLUMN hidden_root uuid,
  ADD COLUMN hidden_by uuid REFERENCES users (id);
CREATE INDEX conversations_hidden_idx ON conversations (hidden_root) WHERE hidden_at IS NOT NULL;
-- The sessions list's keyset: visible conversations by last activity, then
-- id (internal/retrieval sessionsPage).
CREATE INDEX conversations_activity_idx ON conversations (last_activity_at, id) WHERE hidden_at IS NULL;

-- Every other working directory the session named after cwd (Claude's
-- per-record cwd, Codex turn_context cwd and <cwd> tags), accumulated
-- across parses: the admin path rules apply the most restrictive verdict
-- across cwd and these (D18).
ALTER TABLE conversations ADD COLUMN other_cwds text[] NOT NULL DEFAULT '{}';

-- What a device reports of its own file system with each flush: the home
-- directory "~" in an admin path rule means, and where its Claude projects
-- and Codex home are. The server falls back to inferring the home from a
-- transcript's path when a device has not reported it.
ALTER TABLE devices
  ADD COLUMN home text,
  ADD COLUMN claude_projects text,
  ADD COLUMN codex_home text;

-- The sessions a device's harnesses hold open (Claude session files, Devin
-- locks, Codex rollouts a process holds), reported with each flush, and
-- when: retrieval marks them live (the device knows exactly; the server
-- otherwise falls back to recent activity).
ALTER TABLE devices
  ADD COLUMN live_sessions text[] NOT NULL DEFAULT '{}',
  ADD COLUMN live_at timestamptz;
