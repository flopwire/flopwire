-- Device-reported Cowork scope is durable policy evidence, separate from content.
-- Historical uncertainty never disappears when app grants or sources change.
CREATE TABLE session_policy_placements (
  device_id uuid NOT NULL REFERENCES devices(id),
  agent text NOT NULL,
  session_id text NOT NULL,
  placements jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(placements)='array'),
  current_mapping_known boolean NOT NULL,
  evidence_scope text NOT NULL CHECK (evidence_scope IN ('none','mapped','unmapped')),
  client_mode text NOT NULL CHECK (client_mode IN ('allow','local','deny')),
  revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(device_id,agent,session_id)
);
CREATE TABLE source_policy_placements (
  device_id uuid NOT NULL,
  agent text NOT NULL,
  session_id text NOT NULL,
  path text NOT NULL,
  file_id text NOT NULL,
  generation bigint NOT NULL CHECK (generation>=0),
  PRIMARY KEY(device_id,path,file_id,generation,agent,session_id),
  FOREIGN KEY(device_id,agent,session_id) REFERENCES session_policy_placements(device_id,agent,session_id)
);
ALTER TABLE conversations ADD COLUMN hidden_scope text NOT NULL DEFAULT 'user'
  CHECK(hidden_scope IN ('user','device'));

-- Verified aliases and native parent edges survive current-source replacement.
CREATE TABLE session_policy_links (
 device_id uuid NOT NULL,
 agent text NOT NULL,
 session_id text NOT NULL,
 policy_session_id text NOT NULL,
 PRIMARY KEY(device_id,agent,session_id,policy_session_id),
 FOREIGN KEY(device_id,agent,policy_session_id) REFERENCES session_policy_placements(device_id,agent,session_id)
);
