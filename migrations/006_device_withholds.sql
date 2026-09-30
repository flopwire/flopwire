-- Upload credentials may withhold only their bound device's copy.
ALTER TABLE conversation_tombstones ADD COLUMN scope text NOT NULL DEFAULT 'user'
  CHECK (scope IN ('user', 'device'));
ALTER TABLE conversation_tombstones DROP CONSTRAINT conversation_tombstones_user_id_agent_session_id_key;
CREATE UNIQUE INDEX conversation_tombstones_user_session
  ON conversation_tombstones(user_id,agent,session_id) WHERE scope='user';
CREATE UNIQUE INDEX conversation_tombstones_device_session
  ON conversation_tombstones(device_id,agent,session_id) WHERE scope='device';
