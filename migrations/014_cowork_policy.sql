-- Device-reported Cowork scope is durable policy evidence, separate from content.
-- Historical uncertainty never disappears when app grants or sources change.
CREATE TABLE session_policy_placements (
  device_id uuid NOT NULL REFERENCES devices(id),
  agent text NOT NULL,
  session_id text NOT NULL,
  placements jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(placements)='array'),
  current_mapping_known boolean NOT NULL,
  evidence_scope text NOT NULL CHECK (evidence_scope IN ('none','mapped','unmapped')),
  scope_status text NOT NULL DEFAULT 'complete' CHECK(scope_status IN ('complete','limit-held')),
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

-- Ownership is immutable across generations; controlling policy components
-- remain separate so sibling restrictions cannot authorize reassignment.
CREATE TABLE source_policy_identity (
 device_id uuid NOT NULL REFERENCES devices(id),
 path text NOT NULL,
 file_id text NOT NULL,
 owner_agent text NOT NULL,
 owner_session_id text NOT NULL,
 PRIMARY KEY(device_id,path,file_id)
);
CREATE TABLE source_policy_capture_identity (
 device_id uuid NOT NULL,
 path text NOT NULL,
 file_id text NOT NULL,
 capture_session_key text NOT NULL,
 storage_kind text NOT NULL,
 parent_path text NOT NULL,
 parent_file_id text NOT NULL,
 parser_family text NOT NULL,
 PRIMARY KEY(device_id,path,file_id),
 FOREIGN KEY(device_id,path,file_id) REFERENCES source_policy_identity(device_id,path,file_id)
);
