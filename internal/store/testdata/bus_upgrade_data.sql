-- Durable data shared by both authentic 009 variants. No ledger edits.
INSERT INTO users (id,email,name,role,identity_type,created_at) VALUES
 ('00000000-0000-0000-0000-000000000001','sender@example.test','Sender','admin','human','2026-01-01T00:00:00Z'),
 ('00000000-0000-0000-0000-000000000002','recipient@example.test','Recipient','member','human','2026-01-01T00:00:00Z');
INSERT INTO devices (id,user_id,name,platform,created_at) VALUES
 ('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001','sender device','test','2026-01-01T00:00:00Z');
INSERT INTO bus_presence (device_id,user_id,agent,session_id,busy,seen_at) VALUES
 ('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001','codex','sender-session',false,'2026-01-01T00:00:00Z');
INSERT INTO bus_accepts (recipient_user,sender_user,created_at) VALUES
 ('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001','2026-01-01T00:00:00Z');
INSERT INTO bus_messages
 (id,thread_id,reply_to,from_user,from_device,from_agent,from_session,to_user,to_agent,to_session,
  addressed,sender,intent,body,body_sha,refs,state,reason,created_at,expires_at)
 SELECT id,'durable-thread',reply_to,
 '00000000-0000-0000-0000-000000000001'::uuid,
 '00000000-0000-0000-0000-000000000003'::uuid,'codex','sender-session',
 '00000000-0000-0000-0000-000000000002'::uuid,'codex','recipient-session',
 'session','own','inform',body,decode(repeat('ab',32),'hex'),ARRAY['durable-ref'],state,reason,
 '2026-01-01T00:00:00Z'::timestamptz,'2026-01-02T00:00:00Z'::timestamptz
 FROM (VALUES
 ('parent',NULL::text,'durable parent','delivered',''),
 ('reply','parent','durable reply','queued',''),
 ('refusal',NULL,'durable refusal','refused','duplicate'),
 ('held',NULL,'durable held','held',''),
 ('expired',NULL,'durable expired','expired','')) AS fixture(id,reply_to,body,state,reason);
INSERT INTO audit_events (id,actor_id,device_id,action,target_type,target_id,metadata,created_at) VALUES
 ('00000000-0000-0000-0000-000000000004','00000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-000000000003','bus.send','bus_message','parent','{"durable":"parent audit"}','2026-01-01T00:00:00Z'),
 ('00000000-0000-0000-0000-000000000005','00000000-0000-0000-0000-000000000001',
  '00000000-0000-0000-0000-000000000003','bus.deliver','bus_message','','{"ids":["parent"]}','2026-01-01T00:00:00Z'),
 ('00000000-0000-0000-0000-000000000006','00000000-0000-0000-0000-000000000001',
  NULL,'user.login','user','00000000-0000-0000-0000-000000000001','{"durable":"unrelated audit"}','2026-01-01T00:00:00Z');
