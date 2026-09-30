-- target: .local/share/devin/cli/sessions.db
-- Synthetic Devin CLI sessions.db seed. Schema mirrors the real store
-- (columns as of 2026-09). scripts/regen-oracle.sh builds the target file.
-- Adapted from gbasin/agentboard tests/fixtures/fad/seeds/devin.sql (MIT)
-- at commit 4fb640dd9b539413d2ba2fdcf5385a7b51478797.
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  working_directory TEXT NOT NULL,
  backend_type TEXT NOT NULL,
  model TEXT NOT NULL,
  agent_mode TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_activity_at INTEGER NOT NULL,
  title TEXT,
  main_chain_id INTEGER,
  shell_last_seen_index INTEGER DEFAULT 0,
  cogs_json TEXT,
  workspace_dirs TEXT,
  hidden INTEGER NOT NULL DEFAULT 0,
  metadata TEXT
);
CREATE TABLE message_nodes (
  row_id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  node_id INTEGER NOT NULL,
  parent_node_id INTEGER,
  chat_message TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  metadata TEXT,
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  UNIQUE(session_id, node_id)
);

INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, title, main_chain_id, hidden) VALUES
  ('devin-oracle-001', '/tmp/oracle-alpha', 'cli', 'swe-1-7', 'normal', 1790157600, 1790157700, 'Fix login bug', 7, 0),
  ('devin-oracle-002', '/tmp/oracle-beta', 'cli', 'swe-1-7', 'normal', 1790158000, 1790158010, NULL, 2, 0);

-- Session 1: node 4 is a retried reply abandoned when the user edited
-- (off the main chain); the main chain is 1-2-3-5-6-7.
INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES
  ('devin-oracle-001', 1, NULL, '{"message_id":"dm-001","role":"system","content":"You are Devin.","metadata":{"is_user_input":null}}', 1790157600),
  ('devin-oracle-001', 2, 1, '{"message_id":"dm-002","role":"user","content":"fix the login bug","metadata":{"is_user_input":true}}', 1790157601),
  ('devin-oracle-001', 3, 2, '{"message_id":"dm-003","role":"assistant","content":"Reading auth.ts.","thinking":{"thinking":"Start with the auth module.","signature":"c2ln","signature_type":"anthropic"},"tool_calls":[{"id":"call_d1","index":0,"kind":"function","name":"read","arguments":{"path":"auth.ts"}}],"metadata":{}}', 1790157602),
  ('devin-oracle-001', 4, 3, '{"message_id":"dm-004","role":"assistant","content":"(abandoned retry)","metadata":{}}', 1790157603),
  ('devin-oracle-001', 5, 3, '{"message_id":"dm-005","role":"tool","tool_call_id":"call_d1","content":"export function login() { return fetch(url) }","metadata":{}}', 1790157604),
  ('devin-oracle-001', 6, 5, '{"message_id":"dm-006","role":"assistant","content":"login() never awaits fetch; adding await.","metadata":{}}', 1790157605),
  ('devin-oracle-001', 7, 6, '{"message_id":"dm-007","role":"user","content":"now add tests","metadata":{"is_user_input":true}}', 1790157606),
  ('devin-oracle-002', 1, NULL, '{"message_id":"dm-101","role":"user","content":"list the migrations","metadata":{"is_user_input":true}}', 1790158001),
  ('devin-oracle-002', 2, 1, '{"message_id":"dm-102","role":"assistant","content":"There are three migrations.","metadata":{}}', 1790158002);

-- Tables the real store also has. tool_call_state rows update in place.
CREATE TABLE tool_call_state (
  session_id TEXT NOT NULL,
  tool_call_id TEXT NOT NULL,
  tool_call_json TEXT,
  tool_call_update_json TEXT,
  PRIMARY KEY (session_id, tool_call_id),
  FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE TABLE subagent_heads (
  session_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  chain_node_id INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (session_id, agent_id),
  FOREIGN KEY (session_id) REFERENCES sessions(id)
);

-- Session 3: compaction, shaped like the real store. The original chain is
-- 1-2-3-4-(5|6)-7-8-9; nodes 5 and 6 are twin siblings sharing a
-- message_id; node 7 is a cache keepalive "continue" (is_user_input null).
-- Nodes 10-12 are the summarizer's own tree. The new chain attaches to the
-- original prefix at node 2: 13 is the summary, 14 and 15 are copies of 8
-- and 9 (same message_id, compact/prior_node_ids), 16 is new. Main chain:
-- 1-2-13-14-15-16. Node 14's content differs from its original, so the
-- on-chain copy's text must win. Session 4 is hidden (FAD skips it).
-- Session 5 has nodes but no sessions row (a deleted session's leftovers).
INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, title, main_chain_id, hidden) VALUES
  ('devin-oracle-003', '/tmp/oracle-gamma', 'cli', 'swe-1-7', 'normal', 1790160000, 1790160100, 'Port cache to redis', 16, 0),
  ('devin-oracle-004', '/tmp/oracle-delta', 'cli', 'swe-1-7', 'normal', 1790161000, 1790161010, 'Retired session', 2, 1);

INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at, metadata) VALUES
  ('devin-oracle-003', 1, NULL, '{"message_id":"dm-301","role":"system","content":"You are Devin.","metadata":{"is_user_input":null,"telemetry":{"source":"sysprompt","operation":"normal"}}}', 1790160001, NULL),
  ('devin-oracle-003', 2, 1, '{"message_id":"dm-302","role":"user","content":"port the cache to redis","metadata":{"is_user_input":true,"telemetry":{"source":"user","operation":"unknown"}}}', 1790160002, NULL),
  ('devin-oracle-003', 3, 2, '{"message_id":"dm-303","role":"assistant","content":"","tool_calls":[{"id":"exec:0#aaa","index":0,"kind":"function","name":"exec","arguments":{"command":"grep -rn cache src"}}],"metadata":{"telemetry":{"source":"assistant","operation":"inference"}}}', 1790160003, NULL),
  ('devin-oracle-003', 4, 3, '{"message_id":"dm-304","role":"tool","tool_call_id":"exec:0#aaa","content":"src/cache.go:12: type Cache struct","metadata":{"telemetry":{"source":"tool_result","operation":"exec"}}}', 1790160004, NULL),
  ('devin-oracle-003', 5, 4, '{"message_id":"dm-305","role":"assistant","content":"Found the cache type.","metadata":{}}', 1790160005, NULL),
  ('devin-oracle-003', 6, 4, '{"message_id":"dm-305","role":"assistant","content":"Found the cache type.","metadata":{}}', 1790160005, '{"summarized_from":null,"num_tokens_preceding":1200,"is_system_prefix":null}'),
  ('devin-oracle-003', 7, 6, '{"message_id":"dm-306","role":"user","content":"continue","metadata":{"is_user_input":null,"telemetry":{"source":"cache_keepalive","operation":"continue_prompt"}}}', 1790160006, NULL),
  ('devin-oracle-003', 8, 7, '{"message_id":"dm-307","role":"assistant","content":"Swapping the map for a redis client.","tool_calls":[{"id":"edit:0#bbb","index":0,"kind":"function","name":"edit","arguments":{"path":"src/cache.go"}}],"metadata":{}}', 1790160007, NULL),
  ('devin-oracle-003', 9, 8, '{"message_id":"dm-308","role":"tool","tool_call_id":"edit:0#bbb","content":"edited src/cache.go","metadata":{"telemetry":{"source":"tool_result","operation":"edit"}}}', 1790160008, NULL),
  ('devin-oracle-003', 10, NULL, '{"message_id":"dm-309","role":"system","content":"You are a Summarizer.","metadata":{"is_user_input":null}}', 1790160009, '{"summarized_from":null,"num_tokens_preceding":null,"is_system_prefix":true}'),
  ('devin-oracle-003', 11, 10, '{"message_id":"dm-310","role":"user","content":"Conversation to summarize: port the cache to redis","metadata":{"is_user_input":null}}', 1790160010, NULL),
  ('devin-oracle-003', 12, 11, '{"message_id":"dm-311","role":"assistant","content":"Summary: the user asked to port the cache to redis.","metadata":{}}', 1790160011, '{"summarized_from":7,"num_tokens_preceding":null,"is_system_prefix":null}'),
  ('devin-oracle-003', 13, 2, '{"message_id":"dm-312","role":"system","content":"You are continuing work from a previous conversation. Summary: the user asked to port the cache to redis.","metadata":{"is_user_input":null}}', 1790160012, '{"summarized_from":7,"num_tokens_preceding":null,"is_system_prefix":null}'),
  ('devin-oracle-003', 14, 13, '{"message_id":"dm-307","role":"assistant","content":"Swapping the map for a redis client (resumed).","tool_calls":[{"id":"edit:0#bbb","index":0,"kind":"function","name":"edit","arguments":{"path":"src/cache.go"}}],"metadata":{}}', 1790160013, '{"summarized_from":null,"num_tokens_preceding":null,"is_system_prefix":null,"extensions":{"compact/prior_node_ids":[8]}}'),
  ('devin-oracle-003', 15, 14, '{"message_id":"dm-308","role":"tool","tool_call_id":"edit:0#bbb","content":"edited src/cache.go","metadata":{"telemetry":{"source":"tool_result","operation":"edit"}}}', 1790160014, '{"summarized_from":null,"num_tokens_preceding":null,"is_system_prefix":null,"extensions":{"compact/prior_node_ids":[9]}}'),
  ('devin-oracle-003', 16, 15, '{"message_id":"dm-313","role":"assistant","content":"The redis cache is in place.","phase":"final_answer","metadata":{}}', 1790160015, NULL),
  ('devin-oracle-004', 1, NULL, '{"message_id":"dm-401","role":"user","content":"a prompt in a hidden session","metadata":{"is_user_input":true}}', 1790161001, NULL),
  ('devin-oracle-004', 2, 1, '{"message_id":"dm-402","role":"assistant","content":"hidden reply","metadata":{}}', 1790161002, NULL),
  ('devin-oracle-005', 1, NULL, '{"message_id":"dm-501","role":"user","content":"a prompt whose session row is gone","metadata":{"is_user_input":true}}', 1790162001, NULL);

INSERT INTO tool_call_state (session_id, tool_call_id, tool_call_json, tool_call_update_json) VALUES
  ('devin-oracle-001', 'call_d1', '{"toolCallId":"call_d1","title":"Read auth.ts","kind":"read","locations":[{"path":"/tmp/oracle-alpha/auth.ts"}],"rawInput":{"path":"auth.ts"},"_meta":{"cognition.ai/inferenceToolName":"read"}}', '{"toolCallId":"call_d1","status":"completed","_meta":{"cognition.ai/inferenceToolName":"read"}}'),
  ('devin-oracle-003', 'exec:0#aaa', '{"toolCallId":"exec:0#aaa","title":"Ran grep","kind":"execute","rawInput":{"command":"grep -rn cache src"},"_meta":{"cognition.ai/inferenceToolName":"exec"}}', '{"toolCallId":"exec:0#aaa","status":"completed","_meta":{"cognition.ai/cwd":"/tmp/oracle-gamma","terminal_exit":{"terminal_id":"t1","exit_code":1,"signal":null},"cognition.ai/inferenceToolName":"exec"}}'),
  ('devin-oracle-003', 'edit:0#bbb', '{"toolCallId":"edit:0#bbb","title":"Edit cache.go","kind":"edit","locations":[{"path":"/tmp/oracle-gamma/src/cache.go"}],"rawInput":{"path":"src/cache.go"},"_meta":{"cognition.ai/inferenceToolName":"edit"}}', '{"toolCallId":"edit:0#bbb","status":"completed","_meta":{"cognition.ai/inferenceToolName":"edit"}}');
