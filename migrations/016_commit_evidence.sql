-- Rebuildable commit observations. The retained message rows are authoritative.
CREATE TABLE commit_evidence_state (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  index_id uuid NOT NULL DEFAULT gen_random_uuid(),
  cursor_secret bytea NOT NULL DEFAULT decode(replace(gen_random_uuid()::text,'-','')||replace(gen_random_uuid()::text,'-',''),'hex'),
  revision bigint NOT NULL DEFAULT 0,
  watermark bigint NOT NULL DEFAULT 0
);
INSERT INTO commit_evidence_state(singleton) VALUES(true);
CREATE TABLE commit_evidence_dirty (
  conversation_id uuid PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
  limited boolean NOT NULL DEFAULT false,
  retry_after timestamptz
);
INSERT INTO commit_evidence_dirty(conversation_id) SELECT id FROM conversations;
CREATE TABLE commit_evidence (
  id bigserial PRIMARY KEY,
  conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  source_id uuid NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  source_generation bigint NOT NULL,
  fingerprint text NOT NULL,
  observed_hash text COLLATE "C" NOT NULL CHECK (observed_hash ~ '^[0-9a-f]{7,64}$'),
  command_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  result_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  reveal_command_id uuid REFERENCES messages(id) ON DELETE CASCADE,
  reveal_result_id uuid REFERENCES messages(id) ON DELETE CASCADE,
  evidence jsonb NOT NULL,
  UNIQUE(conversation_id,fingerprint)
);
CREATE INDEX commit_evidence_hash_idx ON commit_evidence(observed_hash text_pattern_ops,id);
CREATE INDEX commit_evidence_source_idx ON commit_evidence(source_id);
CREATE INDEX commit_evidence_command_idx ON commit_evidence(command_id);
CREATE INDEX commit_evidence_result_idx ON commit_evidence(result_id);
CREATE INDEX commit_evidence_reveal_command_idx ON commit_evidence(reveal_command_id) WHERE reveal_command_id IS NOT NULL;
CREATE INDEX commit_evidence_reveal_result_idx ON commit_evidence(reveal_result_id) WHERE reveal_result_id IS NOT NULL;

-- Commit-order watermark closes the sequence allocation race: a transaction
-- publishing an older allocated ID after a newer one invalidates open pages.
CREATE FUNCTION flopwire_commit_evidence_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP='INSERT' THEN
    UPDATE commit_evidence_state SET revision=revision+CASE WHEN NEW.id<watermark THEN 1 ELSE 0 END,
      watermark=GREATEST(watermark,NEW.id) WHERE singleton;
  ELSE
    UPDATE commit_evidence_state SET revision=revision+1 WHERE singleton;
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER commit_evidence_revision AFTER INSERT OR DELETE ON commit_evidence
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION flopwire_commit_evidence_revision();

CREATE FUNCTION flopwire_commit_evidence_message() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE cid uuid;
BEGIN
  cid=CASE WHEN TG_OP='DELETE' THEN OLD.conversation_id ELSE NEW.conversation_id END;
  INSERT INTO commit_evidence_dirty(conversation_id) SELECT id FROM conversations WHERE id=cid
    ON CONFLICT(conversation_id) DO UPDATE SET limited=false,retry_after=NULL;
  IF TG_OP='DELETE' OR (TG_OP='UPDATE' AND
    ROW(OLD.conversation_id,OLD.source_id,OLD.source_generation,OLD.native_id,OLD.part,OLD.ordinal,OLD.kind,OLD.tool_name,OLD.tool_call_id,OLD.is_error,
        OLD.text,OLD.enrichment,OLD.superseded,OLD.on_active_path,OLD.line_no,OLD.byte_offset,OLD.byte_len,OLD.locator)
    IS DISTINCT FROM
    ROW(NEW.conversation_id,NEW.source_id,NEW.source_generation,NEW.native_id,NEW.part,NEW.ordinal,NEW.kind,NEW.tool_name,NEW.tool_call_id,NEW.is_error,
        NEW.text,NEW.enrichment,NEW.superseded,NEW.on_active_path,NEW.line_no,NEW.byte_offset,NEW.byte_len,NEW.locator)) THEN
    DELETE FROM commit_evidence WHERE conversation_id=cid;
    IF TG_OP='UPDATE' AND OLD.conversation_id<>cid THEN
      DELETE FROM commit_evidence WHERE conversation_id=OLD.conversation_id;
      INSERT INTO commit_evidence_dirty(conversation_id) SELECT id FROM conversations WHERE id=OLD.conversation_id
        ON CONFLICT(conversation_id) DO UPDATE SET limited=false,retry_after=NULL;
    END IF;
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER commit_evidence_message AFTER INSERT OR UPDATE OR DELETE ON messages
  FOR EACH ROW EXECUTE FUNCTION flopwire_commit_evidence_message();

CREATE FUNCTION flopwire_commit_evidence_visibility() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE commit_evidence_state SET revision=revision+1 WHERE singleton;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER commit_evidence_visibility AFTER UPDATE ON conversations
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
  WHEN (ROW(OLD.hidden_at,OLD.user_id,OLD.device_id,OLD.repo_root,OLD.cwd,OLD.source_id,OLD.agent,OLD.session_id,OLD.branches,OLD.depth,OLD.parent_native_session_id)
    IS DISTINCT FROM ROW(NEW.hidden_at,NEW.user_id,NEW.device_id,NEW.repo_root,NEW.cwd,NEW.source_id,NEW.agent,NEW.session_id,NEW.branches,NEW.depth,NEW.parent_native_session_id))
  EXECUTE FUNCTION flopwire_commit_evidence_visibility();
CREATE CONSTRAINT TRIGGER commit_evidence_device_visibility AFTER UPDATE ON devices
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.name IS DISTINCT FROM NEW.name)
  EXECUTE FUNCTION flopwire_commit_evidence_visibility();
CREATE CONSTRAINT TRIGGER commit_evidence_user_visibility AFTER UPDATE ON users
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.email IS DISTINCT FROM NEW.email)
  EXECUTE FUNCTION flopwire_commit_evidence_visibility();
CREATE TRIGGER commit_evidence_truncate AFTER TRUNCATE ON commit_evidence
  FOR EACH STATEMENT EXECUTE FUNCTION flopwire_commit_evidence_visibility();
CREATE CONSTRAINT TRIGGER commit_evidence_source_visibility AFTER UPDATE ON sources
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
  WHEN (ROW(OLD.tombstoned_at,OLD.remote,OLD.checkout)
    IS DISTINCT FROM ROW(NEW.tombstoned_at,NEW.remote,NEW.checkout))
  EXECUTE FUNCTION flopwire_commit_evidence_visibility();
